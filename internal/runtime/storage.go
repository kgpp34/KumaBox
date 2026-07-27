package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type storageCoordinator struct {
	*Runtime
}

func (s *storageCoordinator) prepare(ctx context.Context, rec *vmstore.VMRecord) error {
	return prepareStorageWithQEMUImg(ctx, rec, s.vmReader.RootDir(), s.qemuImg)
}

func (s *storageCoordinator) removeManagedDirs(rec *vmstore.VMRecord) error {
	return removeManagedDirs(rec, s.vmReader.RootDir())
}

func removeManagedDirs(rec *vmstore.VMRecord, rootDir string) error {
	storageDir := filepath.Join(rootDir, "storage", "vms", rec.ID)
	for _, dir := range []string{rec.RunDir, rec.LogDir, storageDir} {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove managed directory %s: %w", dir, err)
		}
	}
	return nil
}

func prepareStorage(rec *vmstore.VMRecord, rootDir string) error {
	return prepareStorageWithQEMUImg(context.Background(), rec, rootDir, storage.NewQEMUImg("qemu-img"))
}

func prepareStorageWithQEMUImg(ctx context.Context, rec *vmstore.VMRecord, rootDir string, qemuImg *storage.QEMUImg) error {
	if err := vmstore.ValidateStorageContract(rec, rootDir); err != nil {
		return err
	}
	for _, cfg := range rec.StorageConfigs {
		switch cfg.EffectiveRole() {
		case vmstore.StorageRoleLayer:
			if cfg.Path == "" {
				return fmt.Errorf("storage layer %s path must not be empty", cfg.ID)
			}
			info, err := os.Stat(cfg.Path)
			if err != nil {
				return fmt.Errorf("stat storage layer %s: %w", cfg.ID, err)
			}
			if info.IsDir() {
				return fmt.Errorf("storage layer %s must be a file: %s", cfg.ID, cfg.Path)
			}
		case vmstore.StorageRoleCOW:
			if cfg.Base != nil && cfg.Base.Family == "cloudimg" {
				if err := qemuImg.EnsureOverlay(ctx, storage.OverlaySpec{
					Path:       cfg.Path,
					BasePath:   cfg.Base.Path,
					BaseFormat: cfg.Base.Format,
				}); err != nil {
					return fmt.Errorf("prepare cloud image COW %s: %w", cfg.ID, err)
				}
				continue
			}
			if err := prepareCOW(cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareCOW(cfg vmstore.StorageConfig) error {
	if cfg.Path == "" {
		return fmt.Errorf("COW storage path must not be empty")
	}
	sizeBytes := cfg.EffectiveVirtualSize()
	if sizeBytes <= 0 {
		return fmt.Errorf("COW storage %s size must be positive", cfg.ID)
	}
	if info, err := os.Stat(cfg.Path); err == nil && info.Mode().IsRegular() && info.Size() == sizeBytes {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat COW storage %s: %w", cfg.ID, err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return fmt.Errorf("create COW storage dir: %w", err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create COW storage %s: %w", cfg.ID, err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("size COW storage %s: %w", cfg.ID, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close COW storage %s: %w", cfg.ID, err)
	}
	out, err := mkfsExt4(cfg.Path)
	if err != nil {
		_ = os.Remove(cfg.Path)
		return fmt.Errorf("mkfs.ext4 COW storage %s: %w: %s", cfg.ID, err, strings.TrimSpace(string(out)))
	}
	return nil
}
