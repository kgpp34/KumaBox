package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	sandboxfs "github.com/kumabox/kumabox/sandbox"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// SaveSnapshotRequest contains operator labels for one live sandbox capture.
type SaveSnapshotRequest struct {
	// SandboxReference is the source sandbox name or complete ID.
	SandboxReference string
	// Name is an optional unique snapshot lookup key.
	Name string
	// Description is optional operator context stored with the snapshot.
	Description string
}

// SnapshotReporter receives capture stages without controlling the workflow.
type SnapshotReporter interface {
	Status(string) error
	Committed(types.Snapshot) error
}

type snapshotCatalog interface {
	Reserve(context.Context, types.Snapshot) error
	Commit(context.Context, types.SnapshotID, int64) (types.Snapshot, error)
	Forget(context.Context, types.SnapshotID) error
	Resolve(context.Context, string) (types.Snapshot, error)
	List(context.Context) ([]types.Snapshot, error)
	BeginDelete(context.Context, string) (types.Snapshot, error)
	FinalizeDelete(context.Context, types.SnapshotID) error
}

// SnapshotService coordinates sandbox locking, VMM capture, artifact
// publication, and snapshot metadata.
type SnapshotService struct {
	paths        snapshot.Paths
	sandboxPaths sandboxfs.Paths
	sandboxes    sandboxCatalog
	snapshots    snapshotCatalog
	runtimes     *vmm.Registry
	reporter     SnapshotReporter
	newID        func() (types.SnapshotID, error)
	now          func() time.Time
	store        metadata.Store
}

// OpenSnapshots assembles the local snapshot service. The caller must close it.
func OpenSnapshots(ctx context.Context, configuration config.Config, reporter SnapshotReporter) (*SnapshotService, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	imagePaths, err := images.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	sandboxPaths, err := sandboxfs.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	snapshotPaths, err := snapshot.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(imagePaths.Ensure(), sandboxPaths.Ensure(), snapshotPaths.Ensure()); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, imagePaths.MetadataDB(), metadataCollections(), sqlite.Options{
		BusyTimeout: configuration.Metadata.BusyTimeout,
		RetryLimit:  configuration.Metadata.RetryLimit,
	})
	if err != nil {
		return nil, err
	}
	runtimes, err := openVMMRegistry(configuration)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if reporter == nil {
		reporter = discardSnapshotReporter{}
	}
	return &SnapshotService{
		paths: snapshotPaths, sandboxPaths: sandboxPaths,
		sandboxes: sandboxcatalog.New(store, imagecatalog.Reader{}), snapshots: snapshotcatalog.New(store),
		runtimes: runtimes, reporter: reporter, newID: types.NewSnapshotID, now: time.Now, store: store,
	}, nil
}

// Close releases the shared metadata engine.
func (s *SnapshotService) Close() error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.Close()
}

// Save captures native VMM state and the writable COW disk at one paused point.
// The source resumes before artifact publication and metadata commit.
//
//	Running -> lock -> reserve -> stage -> pause/capture/resume -> publish -> ready
//	                              \--- failure: clean stage + reservation ---/
func (s *SnapshotService) Save(ctx context.Context, request SaveSnapshotRequest) (result types.Snapshot, returnErr error) {
	if s == nil || s.sandboxes == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.newID == nil || s.now == nil {
		return types.Snapshot{}, errors.New("snapshot service is not configured")
	}
	if request.SandboxReference == "" {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.reporter.Status("resolving sandbox"); err != nil {
		return types.Snapshot{}, err
	}
	record, err := s.sandboxes.Resolve(ctx, request.SandboxReference)
	if err != nil {
		return types.Snapshot{}, err
	}
	lockPath, err := s.sandboxPaths.Lock(record.ID)
	if err != nil {
		return types.Snapshot{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Snapshot{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Snapshot{}, errdefs.Context(err, "save snapshot", request.SandboxReference, "lock", "retry the snapshot", false)
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(lock.Unlock(context.WithoutCancel(ctx)), "save snapshot", request.SandboxReference, "unlock", "inspect the snapshot before retrying", result.ID != ""))
	}()

	record, err = s.sandboxes.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Snapshot{}, err
	}
	if record.State != types.SandboxStateRunning || record.Generation < 2 {
		return types.Snapshot{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s cannot be snapshotted", record.ID, record.State))
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return types.Snapshot{}, err
	}
	snapshotter, ok := backend.(vmm.Snapshotter)
	if !ok {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support snapshots", record.VMM))
	}
	observation, err := backend.Observe(ctx, record.ID, record.Generation-1)
	if err != nil {
		return types.Snapshot{}, err
	}
	if observation.State != vmm.ProcessRunning {
		return types.Snapshot{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox has no ready VMM process to snapshot"))
	}
	id, err := s.newID()
	if err != nil {
		return types.Snapshot{}, err
	}
	pending := types.Snapshot{
		ID: id, Name: request.Name, Description: request.Description,
		SandboxID: record.ID, SourceGeneration: record.Generation,
		ImageDigest: record.ImageDigest, VMM: record.VMM, Config: record.Config,
		CreatedAt: s.now().UTC(),
	}
	if err := pending.Validate(); err != nil {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := s.reporter.Status("reserving snapshot identity"); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.snapshots.Reserve(ctx, pending); err != nil {
		return types.Snapshot{}, err
	}
	reserved, published := true, false
	defer func() {
		if returnErr == nil || !reserved || published {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		returnErr = errors.Join(returnErr, snapshot.IgnoreAbsence(s.paths.RemoveStage(id)), s.snapshots.Forget(cleanupCtx, id))
	}()
	if err := s.paths.PrepareStage(id); err != nil {
		return types.Snapshot{}, err
	}
	cowSource, err := s.sandboxPaths.COW(record.ID)
	if err != nil {
		return types.Snapshot{}, err
	}
	cowDestination, err := s.paths.StageCOW(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	stage, err := s.paths.Stage(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	if err := s.reporter.Status("capturing VMM and writable disk"); err != nil {
		return types.Snapshot{}, err
	}
	if err := snapshotter.Snapshot(ctx, vmm.SnapshotPlan{
		Process: observation.Process, Destination: stage,
		WritableFiles: []vmm.SnapshotFile{{Source: cowSource, Destination: cowDestination}},
	}); err != nil {
		return types.Snapshot{}, errdefs.Context(err, "save snapshot", request.SandboxReference, "capture", "inspect the running sandbox and retry", false)
	}
	if err := s.reporter.Status("publishing snapshot artifacts"); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.paths.Publish(id); err != nil {
		return types.Snapshot{}, errdefs.Context(err, "save snapshot", request.SandboxReference, "publish", "inspect snapshot storage before retrying", false)
	}
	published = true
	size, err := s.paths.Size(id)
	if err != nil {
		return types.Snapshot{}, errdefs.Context(err, "save snapshot", request.SandboxReference, "measure", "inspect snapshot storage before retrying", true)
	}
	if err := s.reporter.Status("committing snapshot metadata"); err != nil {
		return types.Snapshot{}, errdefs.Context(err, "save snapshot", request.SandboxReference, "report", "inspect snapshot storage before retrying", true)
	}
	result, err = s.snapshots.Commit(ctx, id, size)
	if err != nil {
		return types.Snapshot{}, err
	}
	if err := s.reporter.Committed(result); err != nil {
		return result, errdefs.Context(err, "save snapshot", request.SandboxReference, "report", "snapshot was saved; inspect it before retrying", true)
	}
	return result, nil
}

// List returns every ready snapshot.
func (s *SnapshotService) List(ctx context.Context) ([]types.Snapshot, error) {
	if s == nil || s.snapshots == nil {
		return nil, errors.New("snapshot service is not configured")
	}
	return s.snapshots.List(ctx)
}

// Inspect resolves one ready snapshot by name or complete ID.
func (s *SnapshotService) Inspect(ctx context.Context, reference string) (types.Snapshot, error) {
	if s == nil || s.snapshots == nil {
		return types.Snapshot{}, errors.New("snapshot service is not configured")
	}
	return s.snapshots.Resolve(ctx, reference)
}

// Remove records deletion intent before removing artifacts, then releases the
// metadata name. A failure after intent is retryable with the same reference.
func (s *SnapshotService) Remove(ctx context.Context, reference string) (result types.Snapshot, returnErr error) {
	if s == nil || s.snapshots == nil {
		return types.Snapshot{}, errors.New("snapshot service is not configured")
	}
	record, err := s.snapshots.BeginDelete(ctx, reference)
	if err != nil {
		return types.Snapshot{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return record, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return record, errdefs.Context(err, "remove snapshot", reference, "lock", "retry snapshot removal", true)
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(lock.Unlock(context.WithoutCancel(ctx)), "remove snapshot", reference, "unlock", "retry snapshot removal", true))
	}()
	if err := snapshot.IgnoreAbsence(s.paths.Remove(record.ID)); err != nil {
		return record, errdefs.Context(err, "remove snapshot", reference, "remove artifacts", "retry snapshot removal", true)
	}
	if err := s.snapshots.FinalizeDelete(ctx, record.ID); err != nil {
		return record, err
	}
	return record, nil
}

type discardSnapshotReporter struct{}

func (discardSnapshotReporter) Status(string) error            { return nil }
func (discardSnapshotReporter) Committed(types.Snapshot) error { return nil }
