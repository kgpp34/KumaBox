package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// PublishDir synchronizes a staged directory tree, atomically renames it to an
// absent final path, and synchronizes both parents. Source and destination must
// share a filesystem.
func PublishDir(staged, final string) error {
	if err := CheckPath(staged); err != nil {
		return err
	}
	if err := CheckPath(final); err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("publish directory destination %s already exists", final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := SyncTree(staged); err != nil {
		return err
	}
	if err := os.Rename(staged, final); err != nil {
		return fmt.Errorf("publish directory %s: %w", final, err)
	}
	return errors.Join(syncPath(filepath.Dir(final)), syncPath(filepath.Dir(staged)))
}

// SyncTree flushes regular files and directories from leaves to root. Symlinks
// and special files are rejected because managed artifact trees must be closed.
func SyncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			directories = append(directories, path)
		case info.Mode().IsRegular():
			if err := syncPath(path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("snapshot artifact %s is not a regular file or directory", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, directory := range slices.Backward(directories) {
		if err := syncPath(directory); err != nil {
			return err
		}
	}
	return nil
}
