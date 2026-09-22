package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	sandboxfs "github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/storage"
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
	lifecycle    *SandboxService
}

// OpenSnapshots assembles the local snapshot service. The caller must close it.
func OpenSnapshots(ctx context.Context, configuration config.Config, reporter SnapshotReporter) (*SnapshotService, error) {
	lifecycle, err := OpenSandbox(ctx, configuration, nil)
	if err != nil {
		return nil, err
	}
	snapshotPaths, err := snapshot.NewPaths(configuration.Paths)
	if err != nil {
		return nil, errors.Join(err, lifecycle.Close())
	}
	if err := snapshotPaths.Ensure(); err != nil {
		return nil, errors.Join(err, lifecycle.Close())
	}
	if reporter == nil {
		reporter = discardSnapshotReporter{}
	}
	return &SnapshotService{
		paths: snapshotPaths, sandboxPaths: lifecycle.dependencies.paths,
		sandboxes: lifecycle.dependencies.catalog, snapshots: snapshotcatalog.New(lifecycle.dependencies.store),
		runtimes: lifecycle.dependencies.runtimes, reporter: reporter,
		newID: types.NewSnapshotID, now: time.Now, store: lifecycle.dependencies.store, lifecycle: lifecycle,
	}, nil
}

// Close releases the shared metadata engine.
func (s *SnapshotService) Close() error {
	if s == nil {
		return nil
	}
	if s.lifecycle != nil {
		return s.lifecycle.Close()
	}
	if s.store == nil {
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
		final, pathErr := s.paths.Dir(id)
		_, statErr := os.Stat(final)
		if pathErr == nil && statErr == nil {
			published = true
		}
		return types.Snapshot{}, errdefs.Context(errors.Join(err, pathErr), "save snapshot", request.SandboxReference, "publish", "inspect snapshot storage before retrying", published)
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

// Restore replaces a stopped sandbox's writable disk and launches its native
// VMM snapshot. A live or retained-error source is cleaned through the normal
// stop lifecycle before replacement.
//
//	snapshot lock -> validate + stage disk -> stop -> sandbox lock -> Starting
//	                                                              -> disk replace
//	                                                              -> VMM restore -> Running
func (s *SnapshotService) Restore(ctx context.Context, sandboxReference, snapshotReference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.lifecycle == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("snapshot restore service is not configured")
	}
	if sandboxReference == "" || snapshotReference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX and SNAPSHOT must not be empty"))
	}
	if err := s.reporter.Status("resolving snapshot and sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	capture, err := s.snapshots.Resolve(ctx, snapshotReference)
	if err != nil {
		return types.Sandbox{}, err
	}
	snapshotLockPath, err := s.paths.Lock(capture.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	snapshotLock := filelock.New(snapshotLockPath)
	if err := snapshotLock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "restore sandbox", sandboxReference, "lock snapshot", "retry the restore", false)
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(snapshotLock.Unlock(context.WithoutCancel(ctx)), "restore sandbox", sandboxReference, "unlock snapshot", "inspect the sandbox before retrying", result.Generation > 0))
	}()
	record, err := s.sandboxes.Resolve(ctx, sandboxReference)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := validateRestoreLineage(record, capture); err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	restorer, ok := backend.(vmm.Restorer)
	if !ok {
		return record, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support restore", record.VMM))
	}
	if err := s.reporter.Status("validating snapshot artifacts"); err != nil {
		return record, err
	}
	snapshotDir, err := s.paths.Dir(capture.ID)
	if err != nil {
		return record, err
	}
	snapshotCOW, err := s.paths.COW(capture.ID)
	if err != nil {
		return record, err
	}
	if info, err := os.Lstat(snapshotCOW); err != nil {
		return record, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	} else if !info.Mode().IsRegular() || info.Size() == 0 {
		return record, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("snapshot COW is not a nonempty regular file"))
	}
	if validator, ok := backend.(vmm.RestoreValidator); ok {
		if err := validator.ValidateRestore(ctx, snapshotDir); err != nil {
			return record, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
		}
	}
	if err := s.reporter.Status("checking host runtime"); err != nil {
		return record, err
	}
	if err := backend.Preflight(); err != nil {
		return record, err
	}
	stagedCOW, err := s.paths.RestoreCOW(capture.ID, record.ID)
	if err != nil {
		return record, err
	}
	if err := ignoreNotExist(os.Remove(stagedCOW)); err != nil {
		return record, errdefs.Context(err, "restore sandbox", sandboxReference, "clean staging disk", "inspect snapshot staging storage before retrying", false)
	}
	defer func() { returnErr = errors.Join(returnErr, ignoreNotExist(os.Remove(stagedCOW))) }()
	if err := s.reporter.Status("staging snapshot writable disk"); err != nil {
		return record, err
	}
	if err := storage.CopySparse(stagedCOW, snapshotCOW); err != nil {
		return record, errdefs.Context(err, "restore sandbox", sandboxReference, "stage disk", "verify the snapshot and retry", false)
	}
	stoppedForRestore := false
	defer func() {
		if stoppedForRestore && returnErr != nil {
			returnErr = errdefs.Context(returnErr, "restore sandbox", sandboxReference, "after stop", "inspect the stopped or retained-error sandbox before retrying", true)
		}
	}()
	switch record.State {
	case types.SandboxStateRunning, types.SandboxStateStarting, types.SandboxStateStopping, types.SandboxStateError:
		if err := s.reporter.Status("stopping current sandbox runtime"); err != nil {
			return types.Sandbox{}, err
		}
		if _, err := s.lifecycle.Stop(ctx, record.ID.String()); err != nil {
			return types.Sandbox{}, errdefs.Context(err, "restore sandbox", sandboxReference, "stop", "inspect the sandbox before retrying", true)
		}
		stoppedForRestore = true
	case types.SandboxStateStopped:
	default:
		return types.Sandbox{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s cannot be restored", record.ID, record.State))
	}
	sandboxLockPath, err := s.sandboxPaths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Sandbox{}, err
	}
	sandboxLock := filelock.New(sandboxLockPath)
	if err := sandboxLock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "restore sandbox", sandboxReference, "lock sandbox", "retry the restore", false)
	}
	committed := false
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(sandboxLock.Unlock(context.WithoutCancel(ctx)), "restore sandbox", sandboxReference, "unlock sandbox", "inspect the sandbox before retrying", committed))
	}()
	record, err = s.sandboxes.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	if record.State != types.SandboxStateStopped && record.State != types.SandboxStateError {
		return record, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s changed to state %s before restore", record.ID, record.State))
	}
	if err := validateRestoreLineage(record, capture); err != nil {
		return record, err
	}
	if err := s.reporter.Status("committing starting state"); err != nil {
		return record, err
	}
	starting, err := s.sandboxes.BeginStart(ctx, record.ID, record.Generation, s.now().UTC())
	if err != nil {
		return record, err
	}
	committed = true
	result = starting
	if err := s.lifecycle.recoverNetwork(ctx, starting); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "recover network", err, vmm.Process{})
	}
	liveCOW, err := s.sandboxPaths.COW(record.ID)
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "resolve disk", err, vmm.Process{})
	}
	if err := s.reporter.Status("replacing writable disk"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, vmm.Process{})
	}
	if err := storage.Publish(stagedCOW, liveCOW); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "replace disk", err, vmm.Process{})
	}
	if err := s.reporter.Status("restoring VMM state"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, vmm.Process{})
	}
	process, err := restorer.Restore(ctx, vmm.RestorePlan{
		SandboxID: starting.ID, Generation: starting.Generation, CPUs: starting.Config.CPUs,
		SnapshotDir: snapshotDir, Network: starting.Network,
	})
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "restore VMM", err, process)
	}
	if err := s.reporter.Status("committing running state"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, process)
	}
	running, err := s.sandboxes.MarkRunning(ctx, starting.ID, starting.Generation, s.now().UTC())
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "commit running", err, process)
	}
	return running, nil
}

func validateRestoreLineage(sandbox types.Sandbox, capture types.Snapshot) error {
	if capture.SandboxID != sandbox.ID {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("snapshot belongs to another sandbox"))
	}
	if capture.VMM != sandbox.VMM || capture.ImageDigest != sandbox.ImageDigest || !reflect.DeepEqual(capture.Config, sandbox.Config) {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("snapshot runtime configuration differs from the target sandbox"))
	}
	return nil
}

func ignoreNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type discardSnapshotReporter struct{}

func (discardSnapshotReporter) Status(string) error            { return nil }
func (discardSnapshotReporter) Committed(types.Snapshot) error { return nil }
