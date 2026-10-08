package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/vmm"
)

const restoreCommitMarker = "committed"

// replaceWritableSet keeps the old disks in a sandbox-owned journal until all
// replacements are present. The marker separates an unfinished transaction
// (roll back on recovery) from a complete one (discard old backups).
//
//	stage all -> move old files to backup -> publish new files -> marker
//	             \-- failure: restore old set       \-- crash: keep new set
func replaceWritableSet(files []vmm.SnapshotFile, backupDir string, publish func(string, string) error) error {
	if len(files) == 0 || publish == nil {
		return errors.New("writable disk replacement requires files and publisher")
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		name := filepath.Base(file.Destination)
		if !filepath.IsAbs(file.Source) || !filepath.IsAbs(file.Destination) || file.Source == file.Destination || seen[name] {
			return errors.New("writable disk replacement has invalid or duplicate paths")
		}
		seen[name] = true
	}
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		return fmt.Errorf("create restore disk backup %s: %w", backupDir, err)
	}
	backups := make([]string, 0, len(files))
	rollback := func(cause error) error {
		var rollbackErr error
		for index, backup := range slices.Backward(backups) {
			live := files[index].Destination
			if _, statErr := os.Lstat(backup); errors.Is(statErr, os.ErrNotExist) {
				continue
			} else if statErr != nil {
				rollbackErr = errors.Join(rollbackErr, statErr)
				continue
			}
			rollbackErr = errors.Join(rollbackErr, storage.Publish(backup, live))
		}
		if rollbackErr == nil {
			rollbackErr = os.Remove(backupDir)
		}
		if rollbackErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("recover old disks from %s before starting", backupDir))
		}
		return errors.Join(cause, rollbackErr)
	}
	for _, file := range files {
		backup := filepath.Join(backupDir, filepath.Base(file.Destination))
		backups = append(backups, backup)
		if err := publish(file.Destination, backup); err != nil {
			return rollback(fmt.Errorf("back up disk %s: %w", file.Destination, err))
		}
		if err := publish(file.Source, file.Destination); err != nil {
			return rollback(fmt.Errorf("replace disk %s: %w", file.Destination, err))
		}
	}
	markerStage := filepath.Join(backupDir, restoreCommitMarker+".tmp")
	if err := os.WriteFile(markerStage, []byte("1"), 0o600); err != nil {
		return rollback(errors.Join(err, os.Remove(markerStage)))
	}
	if err := storage.Publish(markerStage, filepath.Join(backupDir, restoreCommitMarker)); err != nil {
		if _, statErr := os.Lstat(filepath.Join(backupDir, restoreCommitMarker)); statErr == nil {
			return fmt.Errorf("restore disk set is complete, but commit sync failed: %w", err)
		}
		return rollback(errors.Join(err, os.Remove(markerStage)))
	}
	return discardCommittedBackups(backupDir)
}

// recoverWritableSet resolves an interrupted disk replacement while the
// sandbox operation lock is held and no VM process is using these files.
func recoverWritableSet(backupDir string, livePaths []string) error {
	if err := storage.CheckPath(backupDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	wanted := make(map[string]string, len(livePaths))
	for _, live := range livePaths {
		wanted[filepath.Base(live)] = live
	}
	committed := false
	for _, entry := range entries {
		if entry.Name() == restoreCommitMarker+".tmp" && entry.Type().IsRegular() {
			continue
		}
		if entry.Name() == restoreCommitMarker && entry.Type().IsRegular() {
			marker, err := os.ReadFile(filepath.Join(backupDir, restoreCommitMarker)) //nolint:gosec // validated private marker
			if err != nil || string(marker) != "1" {
				return errors.Join(err, errors.New("restore backup commit marker is invalid"))
			}
			committed = true
			continue
		}
		if _, ok := wanted[entry.Name()]; !ok || !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected restore backup entry %q in %s", entry.Name(), backupDir)
		}
	}
	if committed {
		return discardCommittedBackups(backupDir)
	}
	for _, entry := range entries {
		if entry.Name() == restoreCommitMarker+".tmp" {
			if err := os.Remove(filepath.Join(backupDir, entry.Name())); err != nil {
				return err
			}
			continue
		}
		backup := filepath.Join(backupDir, entry.Name())
		if err := storage.Publish(backup, wanted[entry.Name()]); err != nil {
			return fmt.Errorf("recover old disk %q: %w", entry.Name(), err)
		}
	}
	for _, live := range livePaths {
		if info, err := os.Lstat(live); err != nil || !info.Mode().IsRegular() {
			return errors.Join(err, fmt.Errorf("disk %s is missing after restore recovery", live))
		}
	}
	return os.Remove(backupDir)
}

// discardCommittedBackups removes the marker last, after syncing backup file
// deletions. A crash during cleanup therefore still identifies the new set as
// complete and cannot turn a partial cleanup into a partial rollback.
func discardCommittedBackups(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == restoreCommitMarker {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected restore backup entry %q", entry.Name())
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	if err := syncRestoreDirectory(directory); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(directory, restoreCommitMarker)); err != nil {
		return err
	}
	if err := syncRestoreDirectory(directory); err != nil {
		return err
	}
	return os.Remove(directory)
}

func syncRestoreDirectory(path string) (returnErr error) {
	directory, err := os.Open(path) //nolint:gosec // private restore journal path is derived from a validated sandbox ID
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, directory.Close()) }()
	return directory.Sync()
}
