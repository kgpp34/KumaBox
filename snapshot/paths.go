// Package snapshot owns persistent snapshot artifacts and their storage
// contracts. Application ordering lives in core and metadata encoding lives in
// snapshot/catalog.
package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const cowName = "cow.raw"

// Paths derives final, staging, and lock paths for snapshot artifacts.
type Paths struct {
	roots storage.Roots
}

// NewPaths validates shared roots without touching the filesystem.
func NewPaths(roots storage.Roots) (Paths, error) {
	validated, err := roots.Validate()
	if err != nil {
		return Paths{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return Paths{roots: validated}, nil
}

// Ensure creates shared artifact, staging, and lock parents.
func (p Paths) Ensure() error {
	for _, path := range []string{p.DataDir(), p.StagingDir(), p.LocksDir()} {
		if err := storage.EnsureDir(path); err != nil {
			return err
		}
	}
	return nil
}

// DataDir contains one immutable directory per ready snapshot.
func (p Paths) DataDir() string { return filepath.Join(p.roots.Data, "snapshots") }

// StagingDir contains unpublished captures safe to remove after failure.
func (p Paths) StagingDir() string { return filepath.Join(p.roots.Data, "staging", "snapshots") }

// LocksDir contains stable snapshot operation locks.
func (p Paths) LocksDir() string { return filepath.Join(p.roots.Run, "locks", "snapshots") }

// Dir returns the published snapshot directory.
func (p Paths) Dir(id types.SnapshotID) (string, error) { return p.idDir(p.DataDir(), id) }

// Stage returns the private unpublished capture directory.
func (p Paths) Stage(id types.SnapshotID) (string, error) { return p.idDir(p.StagingDir(), id) }

// Lock returns the stable operation lock path for one snapshot.
func (p Paths) Lock(id types.SnapshotID) (string, error) {
	if _, err := types.ParseSnapshotID(id.String()); err != nil {
		return "", err
	}
	return storage.Join(p.LocksDir(), id.String()+".lock")
}

// COW returns the captured writable overlay path inside a snapshot directory.
func (p Paths) COW(id types.SnapshotID) (string, error) {
	dir, err := p.Dir(id)
	if err != nil {
		return "", err
	}
	return storage.Join(dir, cowName)
}

// StageCOW returns the unpublished writable overlay path.
func (p Paths) StageCOW(id types.SnapshotID) (string, error) {
	dir, err := p.Stage(id)
	if err != nil {
		return "", err
	}
	return storage.Join(dir, cowName)
}

// RestoreCOW returns a private scratch file used to prepare one sandbox's
// writable disk while its current VMM can continue running.
func (p Paths) RestoreCOW(snapshotID types.SnapshotID, sandboxID types.SandboxID) (string, error) {
	if _, err := types.ParseSnapshotID(snapshotID.String()); err != nil {
		return "", err
	}
	if _, err := types.ParseSandboxID(sandboxID.String()); err != nil {
		return "", err
	}
	return storage.Join(p.StagingDir(), snapshotID.String()+"-restore-"+sandboxID.String()+".raw")
}

// PrepareStage creates an empty private capture directory.
func (p Paths) PrepareStage(id types.SnapshotID) error {
	dir, err := p.Stage(id)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create snapshot staging directory: %w", err)
	}
	return nil
}

// Publish atomically makes a fully synchronized capture visible.
func (p Paths) Publish(id types.SnapshotID) error {
	stage, err := p.Stage(id)
	if err != nil {
		return err
	}
	final, err := p.Dir(id)
	if err != nil {
		return err
	}
	return storage.PublishDir(stage, final)
}

// RemoveStage removes an unpublished capture after a failed save.
func (p Paths) RemoveStage(id types.SnapshotID) error {
	stage, err := p.Stage(id)
	if err != nil {
		return err
	}
	if err := storage.CheckPath(stage); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

// Remove deletes one published artifact directory.
func (p Paths) Remove(id types.SnapshotID) error {
	dir, err := p.Dir(id)
	if err != nil {
		return err
	}
	if err := storage.CheckPath(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove snapshot artifacts: %w", err)
	}
	return nil
}

// Size returns the sum of regular-file logical sizes.
func (p Paths) Size(id types.SnapshotID) (int64, error) {
	dir, err := p.Dir(id)
	if err != nil {
		return 0, err
	}
	var size int64
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (p Paths) idDir(root string, id types.SnapshotID) (string, error) {
	if _, err := types.ParseSnapshotID(id.String()); err != nil {
		return "", err
	}
	return storage.Join(root, id.String())
}

// IgnoreAbsence converts cleanup of an already absent path into success.
func IgnoreAbsence(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
