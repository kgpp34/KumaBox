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
	writable := make([]vmstore.StorageConfig, 0)
	for _, disk := range rec.StorageConfigs {
		role := disk.EffectiveRole()
		if role == vmstore.StorageRoleCOW || role == vmstore.StorageRoleData {
			writable = append(writable, disk)
		}
	}
	if len(writable) == 0 {
		return nil, 0, errors.New("DISK_CONFIG_MISSING: VM has no managed writable disks")
	}

	pending := build.Record()
	disksDir := filepath.Join(pending.StagingDir, "disks")
	if err := os.MkdirAll(disksDir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("create snapshot disks directory: %w", err)
	}
	manifestDisks := make([]DiskManifest, len(writable))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(2)
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
			relPath := filepath.Join("disks", disk.ID+ext)
			result, err := storage.CopyFile(groupCtx, disk.Path, filepath.Join(pending.StagingDir, relPath))
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
	if err := fileutil.WriteJSONAtomic(filepath.Join(pending.StagingDir, "snapshot.json"), manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, 0, fmt.Errorf("write snapshot manifest: %w", err)
	}
	var allocated int64
	for _, disk := range manifestDisks {
		allocated += disk.AllocatedSizeBytes
	}
	return manifest, allocated, nil
}

func validateDiskID(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("DISK_CONFIG_INVALID: disk id %q is not safe", id)
	}
	return nil
}
