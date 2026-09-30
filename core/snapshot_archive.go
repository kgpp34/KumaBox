package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/snapshot"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// Export holds the snapshot operation lock while its immutable files stream to
// the caller. A concurrent remove cannot delete an entry midway through tar.
func (s *SnapshotService) Export(ctx context.Context, reference string, output io.Writer, compress bool) (result types.Snapshot, returnErr error) {
	if s == nil || s.applicationState == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || output == nil {
		return types.Snapshot{}, errors.New("snapshot export service is not configured")
	}
	return s.withSnapshotDirectory(ctx, reference, func(record types.Snapshot, directory string) error {
		if err := s.reporter.Status("streaming snapshot archive"); err != nil {
			return err
		}
		if err := snapshot.WriteArchive(ctx, output, directory, record, compress); err != nil {
			return errdefs.Context(err, "export snapshot", reference, "stream", "discard the incomplete output and retry", false)
		}
		return nil
	})
}

// ExportDirectory reflinks a locked capture into an unpublished directory.
// The caller is responsible for atomically publishing or removing that stage.
func (s *SnapshotService) ExportDirectory(ctx context.Context, reference, destination string) (types.Snapshot, error) {
	if s == nil || s.applicationState == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || destination == "" {
		return types.Snapshot{}, errors.New("snapshot directory export service is not configured")
	}
	return s.withSnapshotDirectory(ctx, reference, func(record types.Snapshot, directory string) error {
		if err := s.reporter.Status("copying snapshot directory"); err != nil {
			return err
		}
		return snapshot.WriteDirectory(ctx, directory, destination, record)
	})
}

func (s *SnapshotService) withSnapshotDirectory(ctx context.Context, reference string, use func(types.Snapshot, string) error) (result types.Snapshot, returnErr error) {
	record, err := s.snapshots.Resolve(ctx, reference)
	if err != nil {
		return types.Snapshot{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return types.Snapshot{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Snapshot{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	record, err = s.snapshots.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Snapshot{}, err
	}
	directory, err := s.paths.Dir(record.ID)
	if err != nil {
		return types.Snapshot{}, err
	}
	if err := s.validateSnapshotArtifacts(ctx, record, directory); err != nil {
		return types.Snapshot{}, err
	}
	if err := use(record, directory); err != nil {
		return types.Snapshot{}, err
	}
	return s.snapshots.Touch(ctx, record.ID, s.now().UTC())
}

// Import stages and verifies a portable archive before reserving a fresh
// identity. The imported record pins its image digest even if the target root
// has not imported that image yet. A failed import leaves neither a ready
// record nor a partial tree.
//
//	stream -> private stage -> validate -> reserve -> publish -> ready
func (s *SnapshotService) Import(ctx context.Context, input io.Reader, name, description string) (result types.Snapshot, returnErr error) {
	if s == nil || s.applicationState == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.newID == nil || s.now == nil || input == nil {
		return types.Snapshot{}, errors.New("snapshot import service is not configured")
	}
	id, err := s.newID()
	if err != nil {
		return types.Snapshot{}, err
	}
	lockPath, err := s.paths.Lock(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Snapshot{}, errdefs.Context(err, "import snapshot", id.String(), "lock", "retry the import", false)
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	if err := s.paths.PrepareStage(id); err != nil {
		return types.Snapshot{}, err
	}
	staged, err := s.paths.Stage(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	reserved, published := false, false
	defer func() {
		if returnErr == nil || result.ID != "" {
			return
		}
		if reserved {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := s.snapshots.Forget(cleanupCtx, id); err != nil {
				returnErr = errors.Join(returnErr, err)
				return
			}
		}
		returnErr = errors.Join(returnErr, snapshot.IgnoreAbsence(s.paths.RemoveStage(id)))
		if published {
			returnErr = errors.Join(returnErr, snapshot.IgnoreAbsence(s.paths.Remove(id)))
		}
	}()
	if err := s.reporter.Status("extracting and checking snapshot archive"); err != nil {
		return types.Snapshot{}, err
	}
	imported, err := snapshot.ReadArchive(ctx, input, staged)
	if err != nil {
		return types.Snapshot{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
	}
	imported.ID, imported.Size, imported.CreatedAt = id, 0, s.now().UTC()
	if name != "" {
		imported.Name = name
	}
	if description != "" {
		imported.Description = description
	}
	if err := imported.Validate(); err != nil {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := s.validateSnapshotArtifacts(ctx, imported, staged); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.reporter.Status("reserving imported snapshot"); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.snapshots.Reserve(ctx, imported); err != nil {
		return types.Snapshot{}, err
	}
	reserved = true
	if err := s.reporter.Status("publishing imported snapshot"); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.paths.Publish(id); err != nil {
		if destination, pathErr := s.paths.Dir(id); pathErr == nil {
			_, statErr := os.Stat(destination)
			published = statErr == nil
		}
		return types.Snapshot{}, errdefs.Context(err, "import snapshot", imported.Name, "publish", "inspect snapshot storage before retrying", published)
	}
	published = true
	size, err := s.paths.Size(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	result, err = s.snapshots.Commit(ctx, id, size, s.now().UTC())
	if err != nil {
		result = types.Snapshot{}
		return types.Snapshot{}, err
	}
	if err := s.reporter.Committed(result); err != nil {
		return result, errdefs.Context(err, "import snapshot", imported.Name, "report", "snapshot was imported; inspect it before retrying", true)
	}
	return result, nil
}

func (s *SnapshotService) validateSnapshotArtifacts(ctx context.Context, record types.Snapshot, directory string) error {
	info, err := os.Lstat(filepath.Join(directory, "cow.raw"))
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Config.Storage {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.Join(err, errors.New("snapshot COW disk is missing, unsafe, or has the wrong logical size")))
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return err
	}
	if validator, ok := backend.(vmm.RestoreValidator); ok {
		if err := validator.ValidateRestore(ctx, directory); err != nil {
			return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
		}
	}
	return nil
}
