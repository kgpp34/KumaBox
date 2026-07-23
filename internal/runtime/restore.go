package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// RestoreOptions defines the new VM identity and runtime attachments.
type RestoreOptions struct {
	Name        string
	CPUs        int
	MemoryBytes int64
	Networks    []string
}

// RestoreSnapshot creates a new CREATED VM from portable writable disk state.
func (r *Runtime) RestoreSnapshot(ctx context.Context, ref string, opts RestoreOptions) (result *vmstore.VMRecord, resultErr error) {
	if opts.Name == "" {
		return nil, errors.New("restore VM name must not be empty")
	}
	if opts.CPUs < 0 {
		return nil, errors.New("restore VM CPUs must be greater than zero")
	}
	if opts.CPUs == 0 {
		opts.CPUs = 1
	}
	if len(opts.Networks) == 0 {
		opts.Networks = []string{"none"}
	}
	operationID, err := r.beginOperation(ctx, operation.KindSnapshotRestoreDisk, ref)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()

	snapshotStore := r.storeSet.Snapshots
	snapshotRec, lease, err := snapshotStore.AcquireRead(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck

	manifest, err := snapshotStore.LoadManifest(ctx, snapshotRec.ID)
	if err != nil {
		return nil, err
	}
	image, err := r.storeSet.Images.Inspect(manifest.Source.ImageID)
	if err != nil {
		return nil, fmt.Errorf("BASE_IMAGE_MISSING: resolve image %s: %w", manifest.Source.ImageID, err)
	}
	req, err := restoreCreateRequest(opts, image, manifest, r.cfg)
	if err != nil {
		return nil, err
	}
	rec, err := r.vmRecords.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.recordVMImageReference(ctx, rec); err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("record restored image reference: %w", err)
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, err
	}
	defer lock.Release() //nolint:errcheck

	ok := false
	defer func() {
		if !ok {
			r.rollbackNetwork(rec)
			_ = removeManagedDirs(rec, r.vmReader.RootDir())
			_ = r.vmRecords.Delete(rec.ID)
		}
	}()
	if err := restoreWritableDisks(ctx, rec, snapshotRec.DataDir, manifest, r.qemuImg); err != nil {
		return nil, err
	}
	if err := r.attachNetwork(rec); err != nil {
		return nil, err
	}
	if updated, inspectErr := r.vmReader.Inspect(rec.ID); inspectErr == nil {
		rec = updated
	}
	if err := prepareStorageWithQEMUImg(ctx, rec, r.vmReader.RootDir(), r.qemuImg); err != nil {
		return nil, err
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		return nil, err
	}
	ok = true
	return r.applyObservation(rec), nil
}

func restoreCreateRequest(opts RestoreOptions, image *imagestore.ImageRecord, manifest *snapshot.Manifest, cfg config.Config) (vmstore.CreateRequest, error) {
	if manifest.Base == nil || image.ID != manifest.Base.ImageID {
		return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: snapshot base does not match local image")
	}
	digest := image.RootDisk.SHA256
	if digest != "" && !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	imageRef := &vmstore.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: manifest.Base.Digest}
	req := vmstore.CreateRequest{Name: opts.Name, CPUs: opts.CPUs, MemoryBytes: opts.MemoryBytes, Networks: opts.Networks, Image: imageRef, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}
	var configs []vmstore.StorageConfig
	switch manifest.Base.Family {
	case "cloudimg":
		if digest != manifest.Base.Digest || image.RootDisk.Format != vmstore.FormatQCOW2 {
			return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local cloud image digest or format differs")
		}
		req.RootDisk = image.RootDisk.Path
		req.Firmware = image.Boot.Firmware
	case "oci":
		if image.OCI == nil {
			return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local image has no OCI metadata")
		}
		manifestDigest := image.OCI.DigestRef
		if _, value, found := strings.Cut(manifestDigest, "@"); found {
			manifestDigest = value
		}
		if manifestDigest != manifest.Base.Digest {
			return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local OCI manifest differs")
		}
		req.Kernel = image.Boot.Kernel
		req.Initrd = image.Boot.Initrd
		req.KernelCmdline = image.Boot.Cmdline
		imageRef.LayerDigests = append([]string(nil), manifest.Base.LayerDigests...)
		if len(image.OCI.Layers) != len(manifest.Base.LayerDigests) {
			return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: OCI layer count differs")
		}
		for i, layer := range image.OCI.Layers {
			if layer.Digest != manifest.Base.LayerDigests[i] || layer.EROFS == nil {
				return vmstore.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: OCI layer digest differs")
			}
			serial := layer.Serial
			if serial == "" {
				serial = vmstore.LayerSerial(i)
			}
			configs = append(configs, vmstore.StorageConfig{ID: vmstore.LayerID(i), Role: vmstore.StorageRoleLayer, Path: layer.EROFS.Path, Readonly: true, Format: vmstore.FormatRaw, Filesystem: vmstore.FilesystemEROFS, Serial: serial, SourceLayer: layer.Digest, VirtualSizeBytes: layer.EROFS.SizeBytes})
		}
	default:
		return vmstore.CreateRequest{}, fmt.Errorf("unsupported snapshot base family %q", manifest.Base.Family)
	}

	diskIDs := make(map[string]struct{}, len(manifest.Disks))
	cowCount := 0
	for _, disk := range manifest.Disks {
		if disk.ID == "" || disk.ID == "." || disk.ID == ".." || strings.ContainsAny(disk.ID, `/\\`) {
			return vmstore.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: unsafe snapshot disk id %q", disk.ID)
		}
		if _, exists := diskIDs[disk.ID]; exists {
			return vmstore.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: duplicate snapshot disk id %q", disk.ID)
		}
		diskIDs[disk.ID] = struct{}{}
		role := vmstore.StorageRole(disk.Role)
		if role != vmstore.StorageRoleCOW && role != vmstore.StorageRoleData {
			return vmstore.CreateRequest{}, errors.New("DISK_CONFIG_INVALID: snapshot contains non-writable payload")
		}
		storageConfig := vmstore.StorageConfig{ID: disk.ID, Role: role, Format: disk.Format, Filesystem: disk.Filesystem, VirtualSizeBytes: disk.VirtualSizeBytes}
		if role == vmstore.StorageRoleCOW {
			cowCount++
			storageConfig.Base = &vmstore.StorageBase{Family: manifest.Base.Family, ImageID: image.ID, Digest: manifest.Base.Digest, Format: manifest.Base.Format, Path: image.RootDisk.Path, LayerDigests: append([]string(nil), manifest.Base.LayerDigests...)}
			if manifest.Base.Family == vmstore.BaseFamilyOCI {
				storageConfig.Serial = vmstore.StorageSerialCOW
			}
		}
		configs = append(configs, storageConfig)
	}
	if cowCount != 1 {
		return vmstore.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: snapshot contains %d root COW disks, want 1", cowCount)
	}
	req.StorageConfigs = configs
	return req, nil
}

func restoreWritableDisks(ctx context.Context, rec *vmstore.VMRecord, snapshotDir string, manifest *snapshot.Manifest, qemuImg *storage.QEMUImg) error {
	byID := make(map[string]snapshot.DiskManifest, len(manifest.Disks))
	for _, disk := range manifest.Disks {
		byID[disk.ID] = disk
	}
	for _, target := range rec.StorageConfigs {
		role := target.EffectiveRole()
		if role != vmstore.StorageRoleCOW && role != vmstore.StorageRoleData {
			continue
		}
		disk, found := byID[target.ID]
		if !found {
			return fmt.Errorf("DISK_CONFIG_MISSING: snapshot disk %s", target.ID)
		}
		source, err := snapshotDiskPath(snapshotDir, disk.Path)
		if err != nil {
			return fmt.Errorf("resolve snapshot disk %s: %w", disk.ID, err)
		}
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
			return fmt.Errorf("create restored disk directory: %w", err)
		}
		result, err := storage.CopyFile(ctx, source, target.Path)
		if err != nil {
			return fmt.Errorf("restore disk %s: %w", disk.ID, err)
		}
		if result.SHA256 != disk.SHA256 {
			return fmt.Errorf("CHECKSUM_MISMATCH: restored disk %s", disk.ID)
		}
		if target.Base != nil && target.Base.Family == "cloudimg" {
			if err := qemuImg.RebaseOverlay(ctx, target.Path, target.Base.Path, target.Base.Format); err != nil {
				return fmt.Errorf("rebase restored disk %s: %w", disk.ID, err)
			}
		}
	}
	return nil
}

func snapshotDiskPath(snapshotDir, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("snapshot disk path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot disk path escapes payload directory")
	}
	path := filepath.Join(snapshotDir, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat snapshot disk: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("snapshot disk is not a regular file")
	}
	return path, nil
}
