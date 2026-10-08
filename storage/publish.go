package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Publish syncs a regular staged file, then atomically renames and syncs both parents.
// Rename refuses a different filesystem; it never falls back to a partial copy.
// Callers serialize destination ownership. A sync error after rename can leave the
// final path present and must not be interpreted as proof that nothing was published.
//
//	staged regular file -> sync file -> rename to final -> sync both parents
//	                                      |
//	                                      +-- final path visible; later sync may fail
func Publish(staged, final string) error {
	if err := CheckPath(staged); err != nil {
		return err
	}
	if err := CheckPath(final); err != nil {
		return err
	}
	info, err := os.Lstat(staged)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("staged artifact %s is not a nonempty regular file", staged)
	}
	if err := EnsureDir(filepath.Dir(final)); err != nil {
		return err
	}
	if err := syncPath(staged); err != nil {
		return err
	}
	if err := os.Rename(staged, final); err != nil {
		return fmt.Errorf("publish %s: %w", final, err)
	}
	return errors.Join(syncPath(filepath.Dir(final)), syncPath(filepath.Dir(staged)))
}

// syncPath opens relative to a directory handle and closes every descriptor while
// preserving sync and close failures. It supports regular files and directories.
func syncPath(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return errors.Join(fmt.Errorf("open for sync %s: %w", path, err), root.Close())
	}
	if err := errors.Join(file.Sync(), file.Close(), root.Close()); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}
