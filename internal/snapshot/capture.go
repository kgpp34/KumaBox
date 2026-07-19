package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// CaptureStopped copies every writable VM disk into a pending snapshot build.
func CaptureStopped(ctx context.Context, build *Build, rec *vmstore.VMRecord) (*Manifest, int64, error) {
	if build == nil || rec == nil {
		return nil, 0, errors.New("snapshot build and VM record are required")
	}
	pending := build.Record()
	manifestDisks, allocated, err := CaptureWritableDisks(ctx, pending.StagingDir, rec)
	if err != nil {
		return nil, 0, err
	}
	writable := writableDisks(rec)

	manifest := newDiskManifest(pending, rec, manifestDisks, writable)
	if err := fileutil.WriteJSONAtomic(filepath.Join(pending.StagingDir, ManifestFile), manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, 0, fmt.Errorf("write snapshot manifest: %w", err)
	}
	return manifest, allocated, nil
}

// CaptureWritableDisks copies every managed writable disk into staging. Calls
// may run while a VM is paused, so copies are bounded and concurrent.
func CaptureWritableDisks(ctx context.Context, stagingDir string, rec *vmstore.VMRecord) ([]DiskManifest, int64, error) {
	disks, _, err := copyWritableDisks(ctx, stagingDir, rec, storage.CopyFile)
	if err != nil {
		return nil, 0, err
	}
	return disks, allocatedSize(disks), nil
}

// StageWritableDisks performs only the copy portion needed inside a running
// snapshot pause window. The returned manifests are incomplete until passed
// to FinalizeWritableDisks after the VM resumes.
func StageWritableDisks(ctx context.Context, stagingDir string, rec *vmstore.VMRecord) ([]DiskManifest, error) {
	disks, _, err := copyWritableDisks(ctx, stagingDir, rec, storage.StageFile)
	return disks, err
}

// FinalizeWritableDisks fsyncs and hashes copies created by
// StageWritableDisks. It is intentionally outside the VM pause window.
func FinalizeWritableDisks(ctx context.Context, stagingDir string, disks []DiskManifest) ([]DiskManifest, int64, error) {
	finalized := append([]DiskManifest(nil), disks...)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(storage.MaxConcurrentFileCopies)
	for i := range finalized {
		i := i
		group.Go(func() error {
			disk := &finalized[i]
			result, err := storage.FinalizeStagedFile(groupCtx, filepath.Join(stagingDir, filepath.FromSlash(disk.Path)), storage.CopyResult{Strategy: disk.CopyStrategy})
			if err != nil {
				return fmt.Errorf("finalize writable disk %s: %w", disk.ID, err)
			}
			disk.VirtualSizeBytes = result.LogicalSizeBytes
			disk.AllocatedSizeBytes = result.AllocatedSizeBytes
			disk.SHA256 = result.SHA256
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, 0, err
	}
	return finalized, allocatedSize(finalized), nil
}

type diskCopier func(context.Context, string, string) (storage.CopyResult, error)

func copyWritableDisks(ctx context.Context, stagingDir string, rec *vmstore.VMRecord, copyDisk diskCopier) ([]DiskManifest, int64, error) {
	if rec == nil {
		return nil, 0, errors.New("VM record is required")
	}
	writable := writableDisks(rec)
	if len(writable) == 0 {
		return nil, 0, errors.New("DISK_CONFIG_MISSING: VM has no managed writable disks")
	}
	disksDir := filepath.Join(stagingDir, DiskPayloadDir)
	if err := os.MkdirAll(disksDir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("create snapshot disks directory: %w", err)
	}
	manifestDisks := make([]DiskManifest, len(writable))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(storage.MaxConcurrentFileCopies)
	for i := range writable {
		i := i
		group.Go(func() error {
			disk := writable[i]
			if err := validateDiskID(disk.ID); err != nil {
				return err
			}
			info, err := os.Stat(disk.Path)
			if err != nil {
				return fmt.Errorf("stat writable disk %s: %w", disk.ID, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("writable disk %s is not a regular file", disk.ID)
			}
			ext := filepath.Ext(disk.Path)
			if ext == "" {
				ext = ".img"
			}
			relPath := filepath.Join(DiskPayloadDir, disk.ID+ext)
			result, err := copyDisk(groupCtx, disk.Path, filepath.Join(stagingDir, relPath))
			if err != nil {
				return fmt.Errorf("capture writable disk %s: %w", disk.ID, err)
			}
			manifestDisks[i] = DiskManifest{
				ID: disk.ID, Role: string(disk.EffectiveRole()), Path: filepath.ToSlash(relPath),
				Format: disk.EffectiveFormat(), Filesystem: disk.Filesystem,
				VirtualSizeBytes: result.LogicalSizeBytes, AllocatedSizeBytes: result.AllocatedSizeBytes,
				SHA256: result.SHA256, CopyStrategy: result.Strategy,
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, 0, err
	}
	return manifestDisks, allocatedSize(manifestDisks), nil
}

func allocatedSize(disks []DiskManifest) int64 {
	var allocated int64
	for _, disk := range disks {
		allocated += disk.AllocatedSizeBytes
	}
	return allocated
}

func writableDisks(rec *vmstore.VMRecord) []vmstore.StorageConfig {
	writable := make([]vmstore.StorageConfig, 0)
	for _, disk := range rec.StorageConfigs {
		role := disk.EffectiveRole()
		if role == vmstore.StorageRoleCOW || role == vmstore.StorageRoleData {
			writable = append(writable, disk)
		}
	}
	return writable
}

func newDiskManifest(pending *Record, rec *vmstore.VMRecord, manifestDisks []DiskManifest, writable []vmstore.StorageConfig) *Manifest {
	manifest := &Manifest{
		SchemaVersion: "kumabox.snapshot.v1", ID: pending.ID, Name: pending.Name,
		Type: "disk", Consistency: "stopped-disk",
		Source: Source{VMID: rec.ID, VMName: rec.Name}, Disks: manifestDisks,
		CreatedAt: time.Now().UTC(),
	}
	if rec.Image != nil {
		manifest.Source.ImageID = rec.Image.ID
		manifest.Source.ImageDigest = rec.Image.Digest
	}
	for _, disk := range writable {
		if disk.EffectiveRole() == vmstore.StorageRoleCOW && disk.Base != nil {
			manifest.Base = &Base{
				Family: disk.Base.Family, ImageID: disk.Base.ImageID, Digest: disk.Base.Digest,
				Format: disk.Base.Format, LayerDigests: append([]string(nil), disk.Base.LayerDigests...),
			}
			break
		}
	}
	return manifest
}

func validateDiskID(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("DISK_CONFIG_INVALID: disk id %q is not safe", id)
	}
	return nil
}
