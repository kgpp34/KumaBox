package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/storage"
)

const maxSnapshotDirectoryEntries = 4096

// ExportDirectory atomically publishes an unpacked snapshot payload. The
// destination must not exist so an interrupted export cannot mix generations.
func (s *Store) ExportDirectory(ctx context.Context, ref, destination string) (err error) {
	if destination == "" || !filepath.IsAbs(destination) {
		return errors.New("snapshot directory export destination must be an absolute path")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("snapshot directory export destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat snapshot directory export destination: %w", err)
	}
	record, lease, err := s.AcquireRead(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create snapshot directory export parent: %w", err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".kumabox-snapshot-dir-*")
	if err != nil {
		return fmt.Errorf("create snapshot directory export staging: %w", err)
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(temporary))
		}
	}()
	if err := copySnapshotTree(ctx, record.DataDir, temporary, false); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("publish snapshot directory export: %w", err)
	}
	published = true
	return syncSnapshotDirectory(filepath.Dir(destination))
}

// ImportDirectory validates an unpacked snapshot in private staging before
// publishing it under a new local identity.
func (s *Store) ImportDirectory(ctx context.Context, source, name, qemuImgBinary string) (record *Record, err error) {
	if source == "" || !filepath.IsAbs(source) {
		return nil, errors.New("snapshot directory import source must be an absolute path")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("stat snapshot directory import source: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("snapshot directory import source must be a real directory")
	}
	build, err := s.Reserve(ctx, name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	if err := copySnapshotTree(ctx, source, build.Record().StagingDir, true); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(build.Record().StagingDir, ManifestFile)
	raw, err := os.ReadFile(manifestPath) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read snapshot directory manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("SNAPSHOT_CORRUPT: decode manifest: %w", err)
	}
	if err := validateDirectoryPayload(ctx, qemuImgBinary, build.Record().StagingDir, &manifest); err != nil {
		return nil, err
	}
	manifest.ID = build.Record().ID
	manifest.Name = name
	if err := fileutil.WriteJSONAtomic(manifestPath, &manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, err
	}
	_, allocated, err := payloadUsage(build.Record().StagingDir)
	if err != nil {
		return nil, fmt.Errorf("measure imported snapshot directory: %w", err)
	}
	return build.FinalizeContext(ctx, allocated)
}

func validateDirectoryPayload(ctx context.Context, qemuImgBinary, root string, manifest *Manifest) error {
	switch manifest.SchemaVersion {
	case "kumabox.snapshot.v1":
		if err := validateManifest(manifest); err != nil {
			return err
		}
		checksums := make(map[string]string, len(manifest.Disks))
		for _, disk := range manifest.Disks {
			checksums[disk.Path] = disk.SHA256
		}
		return validateImportedPayload(qemuImgBinary, root, manifest, checksums)
	case NativeSchemaV2:
		if manifest.Type != NativeType || manifest.Native == nil || manifest.Machine == nil || manifest.Devices == nil {
			return errors.New("SNAPSHOT_CORRUPT: native compatibility metadata is incomplete")
		}
		if err := verifyNativeFiles(ctx, root, manifest); err != nil {
			return err
		}
		return verifyNativeConfig(root, manifest)
	default:
		return fmt.Errorf("SNAPSHOT_CORRUPT: unsupported schema %q", manifest.SchemaVersion)
	}
}

func copySnapshotTree(ctx context.Context, source, destination string, enforceImportLimits bool) error {
	count := 0
	var total int64
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return fmt.Errorf("SNAPSHOT_UNSAFE: path escapes source: %s", path)
		}
		count++
		if enforceImportLimits && count > maxSnapshotDirectoryEntries {
			return errors.New("ARCHIVE_LIMIT_EXCEEDED: too many snapshot directory entries")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode().IsRegular():
			total += info.Size()
			if enforceImportLimits && (info.Size() > maxImportFileSize || total > maxImportTotalSize) {
				return errors.New("ARCHIVE_LIMIT_EXCEEDED: snapshot directory size exceeds limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if _, err := storage.CopyFile(ctx, path, target); err != nil {
				return fmt.Errorf("copy snapshot directory payload %s: %w", relative, err)
			}
			return nil
		default:
			return fmt.Errorf("SNAPSHOT_UNSAFE: unsupported entry %s", relative)
		}
	})
}

func syncSnapshotDirectory(path string) (err error) {
	directory, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open snapshot directory for sync: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync snapshot directory: %w", err)
	}
	return nil
}
