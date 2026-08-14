package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/disk"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vm"
)

// RestoreOptions defines the new VM identity and runtime attachments.
type RestoreOptions struct {
	Name        string
	CPUs        int
	MemoryBytes int64
	Networks    []string
}

// RestoreSnapshot creates a new CREATED VM from portable writable disk state.
func (r *Runtime) RestoreSnapshot(ctx context.Context, ref string, opts RestoreOptions) (result *vm.VMRecord, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

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
	operationID, err := r.beginOperationWithRelated(ctx, operation.KindSnapshotRestoreDisk, ref, ref)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()

	snapshotStore := r.data.Snapshots
	snapshotRec, lease, err := snapshotStore.AcquireRead(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck

	manifest, err := snapshotStore.LoadManifest(ctx, snapshotRec.ID)
	if err != nil {
		return nil, err
	}
	image, err := r.data.Images.Inspect(manifest.Source.ImageID)
	if err != nil {
		return nil, fmt.Errorf("BASE_IMAGE_MISSING: resolve image %s: %w", manifest.Source.ImageID, err)
	}
	imageLock, err := r.resourceGuard.LockEntity(ctx, lock.EntityImage, image.ID)
	if err != nil {
		return nil, err
	}
	defer imageLock.Release() //nolint:errcheck
	image, err = r.data.Images.Inspect(image.ID)
	if err != nil {
		return nil, fmt.Errorf("BASE_IMAGE_MISSING: revalidate image %s: %w", manifest.Source.ImageID, err)
	}
	req, err := restoreCreateRequest(opts, image, manifest, r.cfg)
	if err != nil {
		return nil, err
	}
	rec, err := r.vmRecords.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.bindOperationResource(ctx, operationID, rec.ID); err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("bind restore operation resource: %w", err)
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
			r.network.rollbackNetwork(rec)
			_ = r.disk.removeManagedDirs(rec)
			_ = r.vmRecords.Delete(rec.ID)
		}
	}()
	if err := restoreWritableDisks(ctx, rec, snapshotRec.DataDir, manifest, r.qemuImg); err != nil {
		return nil, err
	}
	if err := r.network.attachNetwork(ctx, rec); err != nil {
		return nil, err
	}
	if updated, inspectErr := r.vmReader.Inspect(rec.ID); inspectErr == nil {
		rec = updated
	}
	if err := r.disk.prepare(ctx, rec); err != nil {
		return nil, err
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		return nil, err
	}
	ok = true
	return r.applyObservation(rec), nil
}

func restoreCreateRequest(opts RestoreOptions, image *image.ImageRecord, manifest *snapshot.Manifest, cfg config.Config) (vm.CreateRequest, error) {
	if manifest.Base == nil || image.ID != manifest.Base.ImageID {
		return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: snapshot base does not match local image")
	}
	digest := image.RootDisk.SHA256
	if digest != "" && !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	imageRef := &vm.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: manifest.Base.Digest}
	req := vm.CreateRequest{Name: opts.Name, CPUs: opts.CPUs, MemoryBytes: opts.MemoryBytes, Networks: opts.Networks, Image: imageRef, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}
	var configs []vm.StorageConfig
	switch manifest.Base.Family {
	case "cloudimg":
		if digest != manifest.Base.Digest || image.RootDisk.Format != vm.FormatQCOW2 {
			return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local cloud image digest or format differs")
		}
		req.RootDisk = image.RootDisk.Path
		req.Firmware = image.Boot.Firmware
	case "oci":
		if image.OCI == nil {
			return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local image has no OCI metadata")
		}
		manifestDigest := image.OCI.DigestRef
		if _, value, found := strings.Cut(manifestDigest, "@"); found {
			manifestDigest = value
		}
		if manifestDigest != manifest.Base.Digest {
			return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: local OCI manifest differs")
		}
		req.Kernel = image.Boot.Kernel
		req.Initrd = image.Boot.Initrd
		req.KernelCmdline = image.Boot.Cmdline
		imageRef.LayerDigests = append([]string(nil), manifest.Base.LayerDigests...)
		if len(image.OCI.Layers) != len(manifest.Base.LayerDigests) {
			return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: OCI layer count differs")
		}
		for i, layer := range image.OCI.Layers {
			if layer.Digest != manifest.Base.LayerDigests[i] || layer.EROFS == nil {
				return vm.CreateRequest{}, errors.New("BASE_IMAGE_MISMATCH: OCI layer digest differs")
			}
			serial := layer.Serial
			if serial == "" {
				serial = vm.LayerSerial(i)
			}
			configs = append(configs, vm.StorageConfig{ID: vm.LayerID(i), Role: vm.StorageRoleLayer, Path: layer.EROFS.Path, Readonly: true, Format: vm.FormatRaw, Filesystem: vm.FilesystemEROFS, Serial: serial, SourceLayer: layer.Digest, VirtualSizeBytes: layer.EROFS.SizeBytes})
		}
	default:
		return vm.CreateRequest{}, fmt.Errorf("unsupported snapshot base family %q", manifest.Base.Family)
	}

	diskIDs := make(map[string]struct{}, len(manifest.Disks))
	cowCount := 0
	for _, disk := range manifest.Disks {
		if disk.ID == "" || disk.ID == "." || disk.ID == ".." || strings.ContainsAny(disk.ID, `/\\`) {
			return vm.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: unsafe snapshot disk id %q", disk.ID)
		}
		if _, exists := diskIDs[disk.ID]; exists {
			return vm.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: duplicate snapshot disk id %q", disk.ID)
		}
		diskIDs[disk.ID] = struct{}{}
		role := vm.StorageRole(disk.Role)
		if role != vm.StorageRoleCOW && role != vm.StorageRoleData {
			return vm.CreateRequest{}, errors.New("DISK_CONFIG_INVALID: snapshot contains non-writable payload")
		}
		storageConfig := vm.StorageConfig{ID: disk.ID, Role: role, Format: disk.Format, Filesystem: disk.Filesystem, VirtualSizeBytes: disk.VirtualSizeBytes}
		if role == vm.StorageRoleCOW {
			cowCount++
			storageConfig.Base = &vm.StorageBase{Family: manifest.Base.Family, ImageID: image.ID, Digest: manifest.Base.Digest, Format: manifest.Base.Format, Path: image.RootDisk.Path, LayerDigests: append([]string(nil), manifest.Base.LayerDigests...)}
			if manifest.Base.Family == vm.BaseFamilyOCI {
				storageConfig.Serial = vm.StorageSerialCOW
			}
		}
		configs = append(configs, storageConfig)
	}
	if cowCount != 1 {
		return vm.CreateRequest{}, fmt.Errorf("DISK_CONFIG_INVALID: snapshot contains %d root COW disks, want 1", cowCount)
	}
	req.StorageConfigs = configs
	return req, nil
}

func restoreWritableDisks(ctx context.Context, rec *vm.VMRecord, snapshotDir string, manifest *snapshot.Manifest, qemuImg *disk.QEMUImg) error {
	byID := make(map[string]snapshot.DiskManifest, len(manifest.Disks))
	for _, disk := range manifest.Disks {
		byID[disk.ID] = disk
	}
	for _, target := range rec.StorageConfigs {
		role := target.EffectiveRole()
		if role != vm.StorageRoleCOW && role != vm.StorageRoleData {
			continue
		}
		manifestDisk, found := byID[target.ID]
		if !found {
			return fmt.Errorf("DISK_CONFIG_MISSING: snapshot disk %s", target.ID)
		}
		source, err := snapshotDiskPath(snapshotDir, manifestDisk.Path)
		if err != nil {
			return fmt.Errorf("resolve snapshot disk %s: %w", manifestDisk.ID, err)
		}
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
			return fmt.Errorf("create restored disk directory: %w", err)
		}
		result, err := disk.CopyFile(ctx, source, target.Path)
		if err != nil {
			return fmt.Errorf("restore disk %s: %w", manifestDisk.ID, err)
		}
		if result.SHA256 != manifestDisk.SHA256 {
			return fmt.Errorf("CHECKSUM_MISMATCH: restored disk %s", manifestDisk.ID)
		}
		if target.Base != nil && target.Base.Family == "cloudimg" {
			if err := qemuImg.RebaseOverlay(ctx, target.Path, target.Base.Path, target.Base.Format); err != nil {
				return fmt.Errorf("rebase restored disk %s: %w", manifestDisk.ID, err)
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
