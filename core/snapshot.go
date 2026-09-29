package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Commit(context.Context, types.SnapshotID, int64, time.Time) (types.Snapshot, error)
	Touch(context.Context, types.SnapshotID, time.Time) (types.Snapshot, error)
	Forget(context.Context, types.SnapshotID) error
	Resolve(context.Context, string) (types.Snapshot, error)
	List(context.Context) ([]types.Snapshot, error)
	BeginDelete(context.Context, string) (types.Snapshot, error)
	FinalizeDelete(context.Context, types.SnapshotID) error
}

// SnapshotService coordinates sandbox locking, VMM capture, artifact
// publication, and snapshot metadata.
type SnapshotService struct {
	configuration config.Config
	paths         snapshot.Paths
	sandboxPaths  sandboxfs.Paths
	sandboxes     sandboxCatalog
	snapshots     snapshotCatalog
	runtimes      *vmm.Registry
	reporter      SnapshotReporter
	newID         func() (types.SnapshotID, error)
	now           func() time.Time
	store         metadata.Store
	lifecycle     *SandboxService
}

// OpenSnapshots assembles the local snapshot service. The caller must close it.
func OpenSnapshots(ctx context.Context, configuration config.Config, reporter SnapshotReporter) (*SnapshotService, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
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
		configuration: configuration,
		paths:         snapshotPaths, sandboxPaths: lifecycle.dependencies.paths,
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
func (s *SnapshotService) Save(ctx context.Context, request SaveSnapshotRequest) (types.Snapshot, error) {
	return s.capture(ctx, request, false)
}

// Hibernate captures and persists a running sandbox while it remains paused,
// then stops its VMM before committing Stopped. Restore resumes that snapshot.
func (s *SnapshotService) Hibernate(ctx context.Context, request SaveSnapshotRequest) (types.Snapshot, error) {
	return s.capture(ctx, request, true)
}

// capture owns the shared reservation and publication contract. The optional
// hibernate tail moves publication inside the VMM pause window.
func (s *SnapshotService) capture(ctx context.Context, request SaveSnapshotRequest, hibernate bool) (result types.Snapshot, returnErr error) {
	if s == nil || s.lifecycle == nil || s.lifecycle.dependencies.images == nil || s.sandboxes == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.newID == nil || s.now == nil {
		return types.Snapshot{}, errors.New("snapshot service is not configured")
	}
	if request.SandboxReference == "" {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	operation := "save snapshot"
	if hibernate {
		operation = "hibernate sandbox"
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
		return types.Snapshot{}, errdefs.Context(err, operation, request.SandboxReference, "lock", "retry the snapshot", false)
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(lock.Unlock(context.WithoutCancel(ctx)), operation, request.SandboxReference, "unlock", "inspect the snapshot before retrying", result.ID != ""))
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
	if hibernate {
		if _, ok := backend.(vmm.Hibernator); !ok {
			return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support hibernate", record.VMM))
		}
	} else if _, ok := backend.(vmm.Snapshotter); !ok {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support snapshots", record.VMM))
	}
	observation, err := backend.Observe(ctx, record.ID, record.Generation-1)
	if err != nil {
		return types.Snapshot{}, err
	}
	if observation.State != vmm.ProcessRunning {
		return types.Snapshot{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox has no ready VMM process to snapshot"))
	}
	image, err := s.lifecycle.dependencies.images.WithAvailable(ctx, record.ImageDigest.String(), func(image types.Image) error {
		if image.ManifestDigest != record.ImageDigest {
			return errors.New("snapshot image differs from the sandbox pin")
		}
		return nil
	})
	if err != nil {
		return types.Snapshot{}, errdefs.Context(err, operation, request.SandboxReference, "image", "restore the pinned image before snapshotting", false)
	}
	id, err := s.newID()
	if err != nil {
		return types.Snapshot{}, err
	}
	pending := types.Snapshot{
		ID: id, Name: request.Name, Description: request.Description,
		SandboxID: record.ID, SourceGeneration: record.Generation,
		ImageDigest: record.ImageDigest, RegistryReference: image.RegistryReference,
		VMM: record.VMM, Config: record.Config,
		CreatedAt: s.now().UTC(),
	}
	if err := pending.Validate(); err != nil {
		return types.Snapshot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	snapshotLockPath, err := s.paths.Lock(id)
	if err != nil {
		return types.Snapshot{}, err
	}
	snapshotLock := filelock.New(snapshotLockPath)
	if err := snapshotLock.Lock(ctx); err != nil {
		return types.Snapshot{}, errdefs.Context(err, operation, id.String(), "lock snapshot", "retry the snapshot", false)
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshotLock.Unlock(context.WithoutCancel(ctx)))
	}()
	if err := s.reporter.Status("reserving snapshot identity"); err != nil {
		return types.Snapshot{}, err
	}
	if err := s.snapshots.Reserve(ctx, pending); err != nil {
		return types.Snapshot{}, err
	}
	reserved, published := true, false
	defer func() {
		if returnErr == nil || !reserved || result.ID != "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// Forget refuses ready records. Only an uncommitted reservation may
		// release its published directory after an uncertain commit error.
		if err := s.snapshots.Forget(cleanupCtx, id); err != nil {
			returnErr = errors.Join(returnErr, err)
			return
		}
		returnErr = errors.Join(returnErr, snapshot.IgnoreAbsence(s.paths.RemoveStage(id)), s.paths.Remove(id))
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
	plan := vmm.SnapshotPlan{
		Process: observation.Process, Destination: stage,
		WritableFiles: []vmm.SnapshotFile{{Source: cowSource, Destination: cowDestination}},
	}
	var stopping types.Sandbox
	persist := func() error {
		if err := s.reporter.Status("publishing snapshot artifacts"); err != nil {
			return err
		}
		if err := s.paths.Publish(id); err != nil {
			final, pathErr := s.paths.Dir(id)
			if pathErr == nil {
				_, statErr := os.Stat(final)
				published = statErr == nil
			}
			return errdefs.Context(errors.Join(err, pathErr), operation, request.SandboxReference, "publish", "inspect snapshot storage before retrying", published)
		}
		published = true
		size, err := s.paths.Size(id)
		if err != nil {
			return errdefs.Context(err, operation, request.SandboxReference, "measure", "inspect snapshot storage before retrying", true)
		}
		if err := s.reporter.Status("committing snapshot metadata"); err != nil {
			return errdefs.Context(err, operation, request.SandboxReference, "report", "inspect snapshot storage before retrying", true)
		}
		result, err = s.snapshots.Commit(ctx, id, size, s.now().UTC())
		if err != nil {
			result = types.Snapshot{}
			return err
		}
		if hibernate {
			if err := s.reporter.Status("committing stopping state"); err != nil {
				return err
			}
			stopping, err = s.sandboxes.BeginStop(ctx, record.ID, record.Generation, s.now().UTC())
			if err != nil {
				return err
			}
		}
		return nil
	}
	if hibernate {
		if err := backend.(vmm.Hibernator).Hibernate(ctx, plan, persist); err != nil {
			return result, errdefs.Context(err, operation, request.SandboxReference, "capture or stop", "inspect the sandbox and snapshot before retrying", result.ID != "" || stopping.Generation > 0)
		}
		if err := s.reporter.Status("cleaning stopped runtime"); err != nil {
			return result, errdefs.Context(err, operation, request.SandboxReference, "report", "retry stop to finish cleanup", true)
		}
		if err := backend.Cleanup(ctx, record.ID); err != nil {
			return result, errdefs.Context(err, operation, request.SandboxReference, "cleanup runtime", "retry stop to finish cleanup", true)
		}
		if err := s.lifecycle.quiesceNetwork(ctx, stopping); err != nil {
			return result, errdefs.Context(err, operation, request.SandboxReference, "quiesce network", "retry stop to finish cleanup", true)
		}
		if _, err := s.sandboxes.MarkStopped(ctx, record.ID, stopping.Generation, types.SandboxStateStopping, s.now().UTC()); err != nil {
			return result, errdefs.Context(err, operation, request.SandboxReference, "mark stopped", "retry stop to finish cleanup", true)
		}
	} else {
		if err := backend.(vmm.Snapshotter).Snapshot(ctx, plan); err != nil {
			return types.Snapshot{}, errdefs.Context(err, operation, request.SandboxReference, "capture", "inspect the running sandbox and retry", false)
		}
		if err := persist(); err != nil {
			return result, err
		}
	}
	if err := s.reporter.Committed(result); err != nil {
		return result, errdefs.Context(err, operation, request.SandboxReference, "report", "snapshot was saved; inspect it before retrying", true)
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

// ListForSandbox resolves a sandbox name or ID before filtering ready captures.
func (s *SnapshotService) ListForSandbox(ctx context.Context, sandboxReference string) ([]types.Snapshot, error) {
	if s == nil || s.sandboxes == nil || s.snapshots == nil {
		return nil, errors.New("snapshot service is not configured")
	}
	owner, err := s.sandboxes.Resolve(ctx, sandboxReference)
	if err != nil {
		return nil, err
	}
	listed, err := s.snapshots.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]types.Snapshot, 0)
	for _, capture := range listed {
		if capture.SandboxID == owner.ID {
			result = append(result, capture)
		}
	}
	return result, nil
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

// RestoreOptions selects an external capture and whether a different source
// sandbox identity may be restored into the target.
type RestoreOptions struct {
	// SourceDirectory selects a portable capture instead of a catalog snapshot.
	SourceDirectory string
	// Force accepts a different source sandbox identity if the shape matches.
	Force bool
	// Pull fetches a missing registry image by the capture's exact digest.
	Pull bool
}

// Restore replaces a stopped sandbox's writable disk and launches its native
// VMM snapshot. A live or retained-error source is cleaned through the normal
// stop lifecycle before replacement.
//
//	snapshot lock -> validate + stage disk -> stop -> sandbox lock -> Starting
//	                                                              -> disk replace
//	                                                              -> VMM restore -> Running
func (s *SnapshotService) Restore(ctx context.Context, sandboxReference, snapshotReference string) (result types.Sandbox, returnErr error) {
	return s.RestoreWithOptions(ctx, sandboxReference, snapshotReference, RestoreOptions{})
}

// RestoreWithOptions also accepts an exported directory. Its native state is
// rebound through the VMM cloner so host paths and NICs can differ from the
// machine that produced the snapshot.
func (s *SnapshotService) RestoreWithOptions(ctx context.Context, sandboxReference, snapshotReference string, options RestoreOptions) (result types.Sandbox, returnErr error) {
	if s == nil || s.lifecycle == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("snapshot restore service is not configured")
	}
	if sandboxReference == "" || (snapshotReference == "") == (options.SourceDirectory == "") || (options.Force && options.SourceDirectory == "") {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("provide SANDBOX and exactly one of SNAPSHOT or --from-dir; --force requires --from-dir"))
	}
	if err := s.reporter.Status("resolving snapshot and sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.sandboxes.Resolve(ctx, sandboxReference)
	if err != nil {
		return types.Sandbox{}, err
	}
	var capture types.Snapshot
	var snapshotDir string
	if options.SourceDirectory != "" {
		stage, stageErr := os.MkdirTemp(s.paths.StagingDir(), "restore-*")
		if stageErr != nil {
			return types.Sandbox{}, stageErr
		}
		defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
		capture, err = snapshot.StageDirectory(ctx, options.SourceDirectory, stage)
		if err != nil {
			return types.Sandbox{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
		}
		snapshotDir = stage
	} else {
		capture, err = s.snapshots.Resolve(ctx, snapshotReference)
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
		snapshotDir, err = s.paths.Dir(capture.ID)
		if err != nil {
			return types.Sandbox{}, err
		}
	}
	if err := validateRestoreSource(record, capture, options); err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	restorer, canRestore := backend.(vmm.Restorer)
	cloner, canClone := backend.(vmm.Cloner)
	if (options.SourceDirectory == "" && !canRestore) || (options.SourceDirectory != "" && !canClone) {
		return record, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support restore", record.VMM))
	}
	if err := s.reporter.Status("validating snapshot artifacts"); err != nil {
		return record, err
	}
	snapshotCOW := filepath.Join(snapshotDir, "cow.raw")
	if info, err := os.Lstat(snapshotCOW); err != nil {
		return record, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	} else if !info.Mode().IsRegular() || info.Size() == 0 || (options.SourceDirectory != "" && info.Size() != capture.Config.Storage) {
		return record, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("snapshot COW has an invalid file type or logical size"))
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
	if options.Pull {
		if err := s.ensureCloneImage(ctx, capture); err != nil {
			return record, err
		}
	}
	if options.SourceDirectory != "" {
		if _, err := s.lifecycle.dependencies.images.WithAvailable(ctx, capture.ImageDigest.String(), func(types.Image) error { return nil }); err != nil {
			return record, errdefs.Context(err, "restore sandbox", sandboxReference, "resolve image", "import or pull the snapshot image before restoring", false)
		}
	}
	var stagedCOW string
	if options.SourceDirectory != "" {
		stagedCOW = snapshotDir + "-cow.raw"
	} else {
		stagedCOW, err = s.paths.RestoreCOW(capture.ID, record.ID)
		if err != nil {
			return record, err
		}
	}
	if err := ignoreNotExist(os.Remove(stagedCOW)); err != nil {
		return record, errdefs.Context(err, "restore sandbox", sandboxReference, "clean staging disk", "inspect snapshot staging storage before retrying", false)
	}
	defer func() { returnErr = errors.Join(returnErr, ignoreNotExist(os.Remove(stagedCOW))) }()
	if err := s.reporter.Status("staging snapshot writable disk"); err != nil {
		return record, err
	}
	if err := storage.CloneFile(stagedCOW, snapshotCOW); err != nil {
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
	if err := validateRestoreSource(record, capture, options); err != nil {
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
	plan := vmm.RestorePlan{
		SandboxID: starting.ID, Generation: starting.Generation, CPUs: starting.Config.CPUs,
		SnapshotDir: snapshotDir, Network: starting.Network,
	}
	var process vmm.Process
	if options.SourceDirectory == "" {
		process, err = restorer.Restore(ctx, plan)
	} else {
		var image types.Image
		image, err = s.lifecycle.dependencies.images.WithAvailable(ctx, capture.ImageDigest.String(), func(types.Image) error { return nil })
		if err == nil {
			var launch vmm.LaunchPlan
			launch, err = s.lifecycle.launchPlan(starting, image)
			if err == nil {
				process, err = cloner.Clone(ctx, vmm.ClonePlan{
					RestorePlan:  plan,
					WritableDisk: liveCOW, ImageDisks: launch.Disks[:len(launch.Disks)-1], Kernel: launch.Kernel, Initrd: launch.Initrd,
				})
			}
		}
	}
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "restore VMM", err, process)
	}
	if err := s.reporter.Status("refreshing guest random state"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, process)
	}
	reseedErr := reseedProcess(ctx, backend, process, options.Force && capture.SandboxID != record.ID)
	if options.SourceDirectory != "" {
		if err := s.reporter.Status("configuring restored guest network"); err != nil {
			return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, process)
		}
		if err := s.configureCloneGuest(ctx, backend, process, starting); err != nil {
			return starting, s.lifecycle.failStart(ctx, backend, starting, "configure guest", err, process)
		}
	}
	if err := s.reporter.Status("committing running state"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, process)
	}
	running, err := s.sandboxes.MarkRunning(ctx, starting.ID, starting.Generation, s.now().UTC())
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "commit running", err, process)
	}
	if options.SourceDirectory == "" {
		if _, err := s.snapshots.Touch(ctx, capture.ID, s.now().UTC()); err != nil {
			return running, errdefs.Context(err, "restore sandbox", sandboxReference, "record snapshot access", "sandbox is running; inspect it before retrying", true)
		}
	}
	if reseedErr != nil {
		return running, errdefs.Context(reseedErr, "restore sandbox", sandboxReference, "reseed guest", "sandbox is running; upgrade the guest agent and run kumabox reseed", true)
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

func validateRestoreSource(sandbox types.Sandbox, capture types.Snapshot, options RestoreOptions) error {
	if options.Force && options.SourceDirectory != "" {
		if capture.VMM != sandbox.VMM || capture.ImageDigest != sandbox.ImageDigest || capture.Config.CPUs != sandbox.Config.CPUs ||
			capture.Config.Memory != sandbox.Config.Memory || capture.Config.Storage != sandbox.Config.Storage || capture.Config.NICs != sandbox.Config.NICs {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("snapshot VMM, image, or resource shape differs from the target sandbox"))
		}
		return nil
	}
	if options.SourceDirectory != "" && capture.SandboxID == sandbox.ID {
		return validateRestoreSource(sandbox, capture, RestoreOptions{SourceDirectory: options.SourceDirectory, Force: true})
	}
	return validateRestoreLineage(sandbox, capture)
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
