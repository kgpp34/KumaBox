package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// DataStore is the optional managed-data-disk capability consumed by sandbox
// creation, validation, and clone. The base Backend remains compatible with
// implementations that only manage the overlay disk.
type DataStore interface {
	PrepareData(context.Context, types.SandboxID, []types.DataDiskSpec) error
	CloneData(context.Context, types.SandboxID, []types.DataDiskSpec, string) error
	CheckData(context.Context, types.SandboxID, []types.DataDiskSpec) error
}

var _ DataStore = (*Ext4)(nil)

// PrepareData creates private sparse disks after the creating record owns the
// directory. Cleanup of any partial set belongs to the existing compensation.
func (d *Ext4) PrepareData(ctx context.Context, id types.SandboxID, specs []types.DataDiskSpec) error {
	if d == nil || d.mkfs == "" {
		return errors.New("managed disk preparer is not configured")
	}
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		path, err := d.paths.DataDisk(id, spec.Name)
		if err != nil {
			return err
		}
		if err := storage.EnsureDir(filepath.Dir(path)); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // managed path under validated sandbox ID
		if err != nil {
			return err
		}
		if err := errors.Join(file.Truncate(spec.Size), file.Close()); err != nil {
			return err
		}
		if spec.FSType == "ext4" {
			output, err := exec.CommandContext(ctx, d.mkfs, "-F", "-m", "0", "-q", "-E", "lazy_itable_init=1,lazy_journal_init=1,discard", path).CombinedOutput() //nolint:gosec // configured formatter and managed path
			if err != nil {
				return fmt.Errorf("format data disk %q: %w: %s", spec.Name, err, strings.TrimSpace(string(output)))
			}
		}
		if err := checkDataDisk(path, spec); err != nil {
			return err
		}
	}
	return nil
}

// CloneData copies all inherited writable data disks from an immutable capture.
func (d *Ext4) CloneData(ctx context.Context, id types.SandboxID, specs []types.DataDiskSpec, sourceDir string) error {
	if d == nil {
		return errors.New("managed disk store is not configured")
	}
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		source := filepath.Join(sourceDir, types.DataDiskFile(spec.Name))
		if err := checkDataDisk(source, spec); err != nil {
			return fmt.Errorf("snapshot data disk %q: %w", spec.Name, err)
		}
		destination, err := d.paths.DataDisk(id, spec.Name)
		if err != nil {
			return err
		}
		if err := storage.EnsureDir(filepath.Dir(destination)); err != nil {
			return err
		}
		if err := storage.CloneFile(destination, source); err != nil {
			return err
		}
	}
	return nil
}

// CheckData detects missing, replaced, or malformed managed disks before boot.
func (d *Ext4) CheckData(ctx context.Context, id types.SandboxID, specs []types.DataDiskSpec) error {
	if d == nil {
		return errors.New("managed disk store is not configured")
	}
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := d.paths.DataDisk(id, spec.Name)
		if err != nil {
			return err
		}
		if err := checkDataDisk(path, spec); err != nil {
			return fmt.Errorf("data disk %q: %w", spec.Name, err)
		}
	}
	return nil
}

func checkDataDisk(path string, spec types.DataDiskSpec) error {
	if err := storage.CheckPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != spec.Size {
		return fmt.Errorf("expected regular %d-byte disk at %s", spec.Size, path)
	}
	if spec.FSType == "ext4" {
		return validate(path, spec.Size)
	}
	return nil
}
