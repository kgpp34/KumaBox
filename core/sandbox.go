package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/disk"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/sandbox"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// CreateSandboxRequest contains user intent before image aliases are resolved.
type CreateSandboxRequest struct {
	// ImageReference is an existing local image alias or manifest digest.
	ImageReference string
	// Config contains the immutable name and guest resource shape.
	Config types.SandboxConfig
	// VMM selects the runtime backend; empty uses Cloud Hypervisor.
	VMM types.VMMType
}

// imageGuard is the image capability consumed by sandbox creation.
type imageGuard interface {
	WithAvailable(context.Context, string, func(types.Image) error) (types.Image, error)
}

// sandboxCreator is the metadata capability consumed by sandbox creation.
type sandboxCreator interface {
	Reserve(context.Context, string, types.Digest, types.Sandbox) error
	MarkCreated(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkError(context.Context, types.SandboxID, uint64, types.SandboxFailure, time.Time) (types.Sandbox, error)
	Forget(context.Context, types.SandboxID, uint64) error
}

// sandboxReader is the metadata capability consumed by sandbox queries and lookup.
type sandboxReader interface {
	Resolve(context.Context, string) (types.Sandbox, error)
	List(context.Context) ([]types.Sandbox, error)
}

// sandboxRemover is the metadata capability consumed by sandbox removal.
type sandboxRemover interface {
	BeginDelete(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	FinalizeDelete(context.Context, types.SandboxID, uint64) error
}

// sandboxLifecycle is the generation-fenced metadata capability used by start and stop.
type sandboxLifecycle interface {
	BeginStart(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkRunning(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkStartError(context.Context, types.SandboxID, uint64, types.SandboxFailure, time.Time) (types.Sandbox, error)
	BeginStop(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkStopped(context.Context, types.SandboxID, uint64, types.SandboxState, time.Time) (types.Sandbox, error)
}

// SandboxReporter receives user-visible stages without controlling workflows.
type SandboxReporter interface {
	Status(string) error
	Committed(types.Sandbox) error
}

// SandboxService owns application ordering and resources for sandbox commands.
type SandboxService struct {
	// paths supplies the stable per-sandbox operation lock path.
	paths sandbox.Paths
	// images closes the verify/pin race with image removal.
	images imageGuard
	// creator commits identity, image references, and create transitions.
	creator sandboxCreator
	// reader supplies consistent sandbox snapshots without changing state.
	reader sandboxReader
	// remover commits generation-fenced delete transitions.
	remover sandboxRemover
	// lifecycle commits generation-fenced start, stop, and recovery transitions.
	lifecycle sandboxLifecycle
	// disks prepares and cleans sandbox-owned writable disks.
	disks disk.Backend
	// imagePaths derives immutable artifacts after the image guard verifies them.
	imagePaths images.Paths
	// runtimes route persisted VMM identities to process adapters.
	runtimes *vmm.Registry
	// defaultVMM selects the runtime when create does not specify one.
	defaultVMM types.VMMType
	// cleanupTimeout bounds compensation that outlives caller cancellation.
	cleanupTimeout time.Duration
	// reporter emits progress independently of command results.
	reporter SandboxReporter
	// newID and now are replaceable in same-package tests.
	newID func() (types.SandboxID, error)
	now   func() time.Time
	// store is the shared metadata engine closed after the command finishes.
	store metadata.Store
}

// newSandboxService connects the explicit capabilities needed by sandbox commands.
func newSandboxService(paths sandbox.Paths, imagePaths images.Paths, images imageGuard, creator sandboxCreator, reader sandboxReader, remover sandboxRemover, lifecycle sandboxLifecycle, disks disk.Backend, runtimes *vmm.Registry, defaultVMM types.VMMType, cleanupTimeout time.Duration, reporter SandboxReporter) *SandboxService {
	if reporter == nil {
		reporter = discardReporter{}
	}
	return &SandboxService{
		paths: paths, imagePaths: imagePaths, images: images, creator: creator, reader: reader,
		remover: remover, lifecycle: lifecycle, disks: disks, runtimes: runtimes,
		defaultVMM: defaultVMM, cleanupTimeout: cleanupTimeout, reporter: reporter,
		newID: types.NewSandboxID, now: time.Now,
	}
}

// OpenSandbox assembles the image guard, metadata catalog, and ext4 COW adapter
// used by sandbox commands. The caller must close the returned service.
//
//	shared SQLite -> image catalog <---- transaction reader ---- sandbox catalog
//	       |              ^                                      |
//	       +---- usage ---+---- image guard + ext4 COW ----------> service
func OpenSandbox(ctx context.Context, configuration config.Config, reporter SandboxReporter) (*SandboxService, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	imagePaths, err := images.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	sandboxPaths, err := sandbox.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	runtimes, err := openVMMRegistry(configuration)
	if err != nil {
		return nil, err
	}
	defaultVMM := configuration.VMM.Default
	if _, err := runtimes.Backend(defaultVMM); err != nil {
		return nil, err
	}
	disks, err := disk.NewExt4(sandboxPaths, configuration.Sandbox.Ext4Binary)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(imagePaths.Ensure(), sandboxPaths.Ensure()); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, imagePaths.MetadataDB(), metadataCollections(), sqlite.Options{
		BusyTimeout: configuration.Metadata.BusyTimeout,
		RetryLimit:  configuration.Metadata.RetryLimit,
	})
	if err != nil {
		return nil, err
	}
	imageCatalog := imagecatalog.New(store, imagecatalog.WithImageUsage(sandboxcatalog.Usage{}))
	sandboxCatalog := sandboxcatalog.New(store, imagecatalog.Reader{})
	service := newSandboxService(
		sandboxPaths, imagePaths, images.NewGuard(imagePaths, imageCatalog), sandboxCatalog,
		sandboxCatalog, sandboxCatalog, sandboxCatalog, disks, runtimes, defaultVMM,
		configuration.Sandbox.CleanupTimeout, reporter,
	)
	service.store = store
	return service, nil
}

// Close releases the shared metadata engine owned by the service.
func (s *SandboxService) Close() error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.Close()
}

// Create reserves identity and image usage before preparing the private disk.
// Only the final generation-fenced transition makes the disk startable.
//
//	validate -> ID lock -> image locks + reservation -> sparse ext4 COW -> Created
//	                            |                         |
//	                            +---- failure cleanup <---+
func (s *SandboxService) Create(ctx context.Context, request CreateSandboxRequest) (result types.Sandbox, returnErr error) {
	if s == nil || s.images == nil || s.creator == nil || s.disks == nil || s.runtimes.Len() == 0 || s.reporter == nil || s.newID == nil || s.now == nil || s.cleanupTimeout <= 0 {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if request.ImageReference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("IMAGE must not be empty"))
	}
	if err := request.Config.Validate(); err != nil {
		return types.Sandbox{}, err
	}
	if request.VMM == "" {
		request.VMM = s.defaultVMM
	}
	if _, err := s.runtimes.Backend(request.VMM); err != nil {
		return types.Sandbox{}, err
	}
	if int(request.Config.CPUs) > runtime.NumCPU() { //nolint:gosec // Config validation bounds CPUs to a small positive value
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("requested %d vCPUs exceeds available host CPUs (%d)", request.Config.CPUs, runtime.NumCPU()))
	}
	if err := s.reporter.Status("resolving and checking image"); err != nil {
		return types.Sandbox{}, err
	}
	id, err := s.newID()
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(id)
	if err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "create sandbox", request.Config.Name, "lock", "retry the create", false)
	}
	defer func() {
		unlockErr := lock.Unlock(context.WithoutCancel(ctx))
		if unlockErr != nil {
			committed := result.State == types.SandboxStateCreated
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "create sandbox", request.Config.Name, "unlock", "inspect the sandbox before retrying", committed)
		}
	}()

	createdAt := s.now().UTC()
	record := types.Sandbox{}
	reserved := false
	_, err = s.images.WithAvailable(ctx, request.ImageReference, func(image types.Image) error {
		record = types.Sandbox{
			ID: id, Config: request.Config, ImageDigest: image.ManifestDigest,
			VMM:   request.VMM,
			State: types.SandboxStateCreating, Generation: 1,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := s.creator.Reserve(ctx, request.ImageReference, image.ManifestDigest, record); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		if reserved {
			return types.Sandbox{}, s.compensate(ctx, record, "image unlock", err)
		}
		return types.Sandbox{}, errdefs.Context(err, "create sandbox", request.Config.Name, "reserve", "check the image and sandbox name", false)
	}
	if err := s.reporter.Status("creating sparse ext4 disk"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	if err := s.disks.Prepare(ctx, id, request.Config.Storage); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "disk", err)
	}
	if err := s.reporter.Status("committing created state"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	created, err := s.creator.MarkCreated(ctx, id, record.Generation, s.now().UTC())
	if err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "commit", err)
	}
	result = created
	if err := s.reporter.Committed(created); err != nil {
		return created, errdefs.Context(err, "create sandbox", request.Config.Name, "report", "sandbox was created; inspect it before retrying", true)
	}
	return created, nil
}

// List returns a consistent sandbox snapshot. Unless includeAll is true, only
// states associated with an active VMM operation are returned.
func (s *SandboxService) List(ctx context.Context, includeAll bool) ([]types.Sandbox, error) {
	if s == nil || s.reader == nil {
		return nil, errors.New("sandbox service is not configured")
	}
	records, err := s.reader.List(ctx)
	if err != nil {
		return nil, err
	}
	if includeAll {
		return records, nil
	}
	active := make([]types.Sandbox, 0, len(records))
	for _, record := range records {
		switch record.State {
		case types.SandboxStateStarting, types.SandboxStateRunning, types.SandboxStateStopping:
			active = append(active, record)
		}
	}
	return active, nil
}

// Inspect resolves one sandbox snapshot without changing persistent or runtime state.
// Runtime observation will be added here when the VMM lifecycle is available.
func (s *SandboxService) Inspect(ctx context.Context, reference string) (types.Sandbox, error) {
	if s == nil || s.reader == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	return s.reader.Resolve(ctx, reference)
}

// Start validates persistent inputs, recovers an interrupted launch when
// possible, and commits Running only after the exact VMM reports readiness.
//
//	resolve + lock -> verify image/COW -> Starting -> launch -> API Running
//	                        ^               |                    |
//	                        +---- retry ----+-------- CAS Running+
//	                                        |
//	                              abort + retained Error
func (s *SandboxService) Start(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.reader == nil || s.lifecycle == nil || s.images == nil || s.disks == nil || s.runtimes.Len() == 0 || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.reader.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "start sandbox", reference, "lock", "retry the start", false)
	}
	committed := false
	defer func() {
		if unlockErr := lock.Unlock(context.WithoutCancel(ctx)); unlockErr != nil {
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "start sandbox", reference, "unlock", "inspect the sandbox before retrying", committed)
		}
	}()

	// The first resolve selects the lock; this second resolve is authoritative.
	record, err = s.reader.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	beforeRecovery := record
	result, done, err := s.recoverStart(ctx, backend, record)
	if err != nil {
		return types.Sandbox{}, errdefs.Context(err, "start sandbox", reference, "recover runtime", "inspect the sandbox and VMM log before retrying", false)
	}
	committed = result.Generation != beforeRecovery.Generation || result.State != beforeRecovery.State
	if done {
		committed = true
		if err := s.reporter.Committed(result); err != nil {
			return result, errdefs.Context(err, "start sandbox", reference, "report", "sandbox is running; inspect it before retrying", true)
		}
		return result, nil
	}
	record = result
	failBeforeLaunch := func(phase string, cause error) error {
		if record.State == types.SandboxStateStarting {
			return s.failStart(ctx, backend, record, phase, cause, vmm.Process{})
		}
		return errdefs.Context(cause, "start sandbox", reference, phase, "fix the validation failure and retry", committed)
	}

	if err := s.reporter.Status("checking host runtime"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	if err := backend.Preflight(); err != nil {
		return record, failBeforeLaunch("host preflight", err)
	}
	if int(record.Config.CPUs) > runtime.NumCPU() {
		return record, failBeforeLaunch("host capacity", errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("requested %d vCPUs exceeds available host CPUs (%d)", record.Config.CPUs, runtime.NumCPU())))
	}
	if err := s.reporter.Status("verifying image and sandbox disk"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	var plan vmm.LaunchPlan
	_, err = s.images.WithAvailable(ctx, record.ImageDigest.String(), func(image types.Image) error {
		var buildErr error
		plan, buildErr = s.launchPlan(record, image)
		if buildErr != nil {
			return buildErr
		}
		return s.disks.Check(ctx, record.ID, record.Config.Storage)
	})
	if err != nil {
		return record, failBeforeLaunch("validate artifacts", err)
	}

	if err := s.reporter.Status("committing starting state"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	starting, err := s.lifecycle.BeginStart(ctx, record.ID, record.Generation, s.now().UTC())
	if err != nil {
		return record, errdefs.Context(err, "start sandbox", reference, "mark starting", "inspect the sandbox before retrying", committed)
	}
	committed = true
	result = starting
	plan.Generation = starting.Generation
	if err := plan.Validate(); err != nil {
		return starting, s.failStart(ctx, backend, starting, "build launch plan", err, vmm.Process{})
	}
	if err := s.reporter.Status("launching " + string(backend.Type())); err != nil {
		return starting, s.failStart(ctx, backend, starting, "report", err, vmm.Process{})
	}
	process, err := backend.Launch(ctx, plan)
	if err != nil {
		return starting, s.failStart(ctx, backend, starting, "launch VMM", err, process)
	}
	if err := s.reporter.Status("committing running state"); err != nil {
		return starting, s.failStart(ctx, backend, starting, "report", err, process)
	}
	running, err := s.lifecycle.MarkRunning(ctx, starting.ID, starting.Generation, s.now().UTC())
	if err != nil {
		return starting, s.failStart(ctx, backend, starting, "commit running", err, process)
	}
	result = running
	if err := s.reporter.Committed(running); err != nil {
		return running, errdefs.Context(err, "start sandbox", reference, "report", "sandbox is running; inspect it before retrying", true)
	}
	return running, nil
}

// recoverStart reconciles durable lifecycle state with an owned process. The
// returned boolean is true when Running is already established.
func (s *SandboxService) recoverStart(ctx context.Context, backend vmm.Backend, record types.Sandbox) (types.Sandbox, bool, error) {
	switch record.State {
	case types.SandboxStateCreating, types.SandboxStateStopping, types.SandboxStateDeleting:
		return record, false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s cannot start", record.ID, record.State))
	}
	expected := record.Generation
	if record.State == types.SandboxStateRunning {
		if record.Generation < 2 {
			return record, false, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("running sandbox has no Starting generation"))
		}
		expected--
	}
	if err := s.reporter.Status("checking existing runtime"); err != nil {
		return record, false, err
	}
	observation, err := backend.Observe(ctx, record.ID, expected)
	if err != nil {
		return record, false, err
	}
	switch record.State {
	case types.SandboxStateRunning:
		switch observation.State {
		case vmm.ProcessRunning:
			return record, true, nil
		case vmm.ProcessStarting:
			if err := backend.WaitReady(ctx, observation.Process); err != nil {
				return record, false, err
			}
			return record, true, nil
		case vmm.ProcessAbsent:
			if err := backend.Cleanup(ctx, record.ID); err != nil {
				return record, false, err
			}
			stopped, err := s.lifecycle.MarkStopped(ctx, record.ID, record.Generation, types.SandboxStateRunning, s.now().UTC())
			return stopped, false, err
		}
	case types.SandboxStateStarting:
		switch observation.State {
		case vmm.ProcessRunning:
			running, err := s.lifecycle.MarkRunning(ctx, record.ID, record.Generation, s.now().UTC())
			return running, err == nil, err
		case vmm.ProcessStarting:
			if err := backend.WaitReady(ctx, observation.Process); err != nil {
				return record, false, s.failStart(ctx, backend, record, "recover VMM", err, observation.Process)
			}
			running, err := s.lifecycle.MarkRunning(ctx, record.ID, record.Generation, s.now().UTC())
			if err != nil {
				return record, false, s.failStart(ctx, backend, record, "commit recovered VMM", err, observation.Process)
			}
			return running, true, nil
		case vmm.ProcessAbsent:
			if err := backend.Cleanup(ctx, record.ID); err != nil {
				return record, false, err
			}
			return record, false, nil
		}
	default:
		if observation.State != vmm.ProcessAbsent {
			return record, false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s has a live VMM while state is %s", record.ID, record.State))
		}
		if err := backend.Cleanup(ctx, record.ID); err != nil {
			return record, false, err
		}
		return record, false, nil
	}
	return record, false, errdefs.New(errdefs.ClassInternal, errdefs.CodeInternal, fmt.Errorf("unknown VMM observation %q", observation.State))
}

// launchPlan maps a pinned image and sandbox resource request into the public
// overlay-v1 guest ABI. It does not inspect or mutate host files.
func (s *SandboxService) launchPlan(record types.Sandbox, image types.Image) (vmm.LaunchPlan, error) {
	if image.ManifestDigest != record.ImageDigest {
		return vmm.LaunchPlan{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("resolved image differs from the sandbox pin"))
	}
	if image.Platform.OS != "linux" || image.Platform.Architecture != runtime.GOARCH {
		return vmm.LaunchPlan{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeImageIncompatible, fmt.Errorf("image platform %s/%s cannot run on %s/%s", image.Platform.OS, image.Platform.Architecture, runtime.GOOS, runtime.GOARCH))
	}
	if image.Boot.Profile != types.BootProfileOverlayV1 {
		return vmm.LaunchPlan{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeImageIncompatible, fmt.Errorf("image boot profile %q is not supported; expected %q", image.Boot.Profile, types.BootProfileOverlayV1))
	}
	kernel, err := s.imagePaths.BootFile(image.Boot.KernelLayer, image.Boot.KernelFile)
	if err != nil {
		return vmm.LaunchPlan{}, err
	}
	initrd, err := s.imagePaths.BootFile(image.Boot.InitrdLayer, image.Boot.InitrdFile)
	if err != nil {
		return vmm.LaunchPlan{}, err
	}
	cmdline, err := vmm.OverlayV1Cmdline(vmm.OverlayV1Config{LayerCount: len(image.Layers), Hostname: record.Config.Name})
	if err != nil {
		return vmm.LaunchPlan{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeImageIncompatible, err)
	}
	disks := make([]vmm.Disk, 0, len(image.Layers)+1)
	for position, layer := range image.Layers {
		disks = append(disks, vmm.Disk{Path: s.imagePaths.EROFS(layer.SourceDigest), Serial: fmt.Sprintf("%s%d", vmm.LayerSerialPrefix, position), ReadOnly: true})
	}
	cow, err := s.paths.COW(record.ID)
	if err != nil {
		return vmm.LaunchPlan{}, err
	}
	disks = append(disks, vmm.Disk{Path: cow, Serial: vmm.COWSerial})
	return vmm.LaunchPlan{
		SandboxID: record.ID, CPUs: record.Config.CPUs, Memory: record.Config.Memory,
		BootProfile: image.Boot.Profile, Kernel: kernel, Initrd: initrd, Cmdline: cmdline, Disks: disks,
	}, nil
}

// failStart cleans only the exact process identity (when available) and retains
// an Error record so the next start or removal has an explicit owner.
func (s *SandboxService) failStart(ctx context.Context, backend vmm.Backend, starting types.Sandbox, phase string, cause error, process vmm.Process) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	var cleanupErr error
	if process.PID > 0 {
		cleanupErr = backend.Abort(cleanupCtx, process)
	} else {
		cleanupErr = backend.Cleanup(cleanupCtx, starting.ID)
	}
	failureCause := errors.Join(cause, cleanupErr)
	failure := types.SandboxFailure{Phase: phase, Message: failureCause.Error()}
	_, markErr := s.lifecycle.MarkStartError(cleanupCtx, starting.ID, starting.Generation, failure, s.now().UTC())
	return errdefs.Context(errors.Join(failureCause, markErr), "start sandbox", starting.Config.Name, phase, "inspect the retained error sandbox and VMM log", true)
}

// Stop terminates the exact VMM process owned by one sandbox and commits
// Stopped only after process absence and runtime cleanup are proven.
//
//	Running + live VMM -> Stopping -> TERM -> 5s -> KILL -> cleanup -> Stopped
//	Starting/Stopping  ----- retry resumes the owned process generation -----^
//	Running + no VMM  --------------------- cleanup ------------------------^
func (s *SandboxService) Stop(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.reader == nil || s.lifecycle == nil || s.runtimes.Len() == 0 || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.reader.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "stop sandbox", reference, "lock", "retry the stop", false)
	}
	committed := false
	defer func() {
		if unlockErr := lock.Unlock(context.WithoutCancel(ctx)); unlockErr != nil {
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "stop sandbox", reference, "unlock", "inspect the sandbox before retrying", committed)
		}
	}()

	// The first resolve selects the lock; this second resolve is authoritative.
	record, err = s.reader.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	result = record
	if record.State == types.SandboxStateCreating || record.State == types.SandboxStateDeleting {
		return record, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s cannot stop", record.ID, record.State))
	}
	if record.State == types.SandboxStateCreated || record.State == types.SandboxStateStopped {
		if err := s.reporter.Status("cleaning stale runtime state"); err != nil {
			return record, err
		}
		if err := backend.Cleanup(ctx, record.ID); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "cleanup runtime", "inspect the runtime scope before retrying", false)
		}
		if err := s.reporter.Committed(record); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "sandbox is not running", false)
		}
		return record, nil
	}

	processGeneration, err := stopProcessGeneration(record)
	if err != nil {
		return record, err
	}
	if err := s.reporter.Status("checking existing runtime"); err != nil {
		return record, err
	}
	process, exists, err := backend.Locate(ctx, record.ID, processGeneration)
	if err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "observe runtime", "inspect the sandbox runtime before retrying", false)
	}

	if record.State == types.SandboxStateRunning && exists {
		if err := s.reporter.Status("committing stopping state"); err != nil {
			return record, err
		}
		record, err = s.lifecycle.BeginStop(ctx, record.ID, record.Generation, s.now().UTC())
		if err != nil {
			return result, errdefs.Context(err, "stop sandbox", reference, "mark stopping", "inspect the sandbox before retrying", false)
		}
		result, committed = record, true
	}

	if exists {
		if err := s.reporter.Status("stopping " + string(backend.Type())); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to resume Stopping", committed)
		}
		if err := backend.Stop(ctx, process); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "stop VMM", "retry the stop; the retained state preserves ownership", committed)
		}
	}
	if err := s.reporter.Status("cleaning runtime state"); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to finish cleanup", committed)
	}
	if err := backend.Cleanup(ctx, record.ID); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "cleanup runtime", "retry the stop to finish cleanup", committed)
	}

	// Error retains the original start/create diagnostic after any residual VMM
	// is gone. It can be removed or started explicitly by the next command.
	if record.State == types.SandboxStateError {
		if err := s.reporter.Committed(record); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "the VMM is stopped; inspect the retained error", committed)
		}
		return record, nil
	}
	if err := s.reporter.Status("committing stopped state"); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to commit process absence", committed)
	}
	stopped, err := s.lifecycle.MarkStopped(ctx, record.ID, record.Generation, record.State, s.now().UTC())
	if err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "mark stopped", "inspect the sandbox before retrying", committed)
	}
	result, committed = stopped, true
	if err := s.reporter.Committed(stopped); err != nil {
		return stopped, errdefs.Context(err, "stop sandbox", reference, "report", "sandbox is stopped; inspect it before retrying", true)
	}
	return stopped, nil
}

// stopProcessGeneration maps durable lifecycle transitions back to the
// Starting generation stored in process identity.
func stopProcessGeneration(record types.Sandbox) (uint64, error) {
	var offset uint64
	switch record.State {
	case types.SandboxStateStarting:
		offset = 0
	case types.SandboxStateRunning, types.SandboxStateError:
		offset = 1
	case types.SandboxStateStopping:
		offset = 2
	default:
		return 0, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s has no stoppable process generation", record.ID, record.State))
	}
	if record.Generation <= offset {
		return 0, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("sandbox %s state %s has invalid generation %d", record.ID, record.State, record.Generation))
	}
	return record.Generation - offset, nil
}

// Console opens the current direct-boot PTY after proving the sandbox record
// and VMM process refer to the same Running generation.
//
//	resolve -> lock -> reread Running -> locate exact process -> unlock -> open PTY
//	                                                                      |
//	                                             caller owns console session
func (s *SandboxService) Console(ctx context.Context, reference string) (io.ReadWriteCloser, error) {
	backend, process, err := s.locateRunning(ctx, reference, "open sandbox console")
	if err != nil {
		return nil, err
	}
	connection, err := backend.Console(ctx, process)
	if err != nil {
		return nil, errdefs.Context(err, "open sandbox console", reference, "open PTY", "inspect the VMM log and retry", false)
	}
	return connection, nil
}

// Exec runs one command through the guest agent after resolving an exact live
// VMM process. The operation lock is released before network I/O and command
// execution so stop can always make progress.
//
//	resolve + lock -> Running generation -> locate process -> unlock
//	                                                        |
//	                          vsock -> agent stream -> exit code
func (s *SandboxService) Exec(ctx context.Context, reference string, config types.ExecConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := config.Validate(); err != nil {
		return 0, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	backend, process, err := s.locateRunning(ctx, reference, "execute sandbox command")
	if err != nil {
		return 0, err
	}
	connection, err := backend.DialVsock(ctx, process, agent.Port)
	if err != nil {
		return 0, errdefs.Context(err, "execute sandbox command", reference, "connect guest agent", "the guest agent may still be starting; retry shortly or inspect its service", false)
	}
	defer connection.Close() //nolint:errcheck // closing a completed read/write session cannot change the guest command result
	if !config.Interactive {
		stdin = nil
	}
	exitCode, err := agent.Run(ctx, connection, config.Args, config.Environment(), stdin, stdout, stderr)
	if err != nil {
		return 0, errdefs.Context(err, "execute sandbox command", reference, "run guest command", "inspect the guest agent and retry", false)
	}
	return exitCode, nil
}

// locateRunning returns an identity-checked VMM generation. It holds the
// sandbox operation lock only while persistent and process facts are resolved.
func (s *SandboxService) locateRunning(ctx context.Context, reference, operation string) (backend vmm.Backend, process vmm.Process, returnErr error) {
	if s == nil || s.reader == nil || s.runtimes.Len() == 0 {
		return nil, vmm.Process{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	record, err := s.reader.Resolve(ctx, reference)
	if err != nil {
		return nil, vmm.Process{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return nil, vmm.Process{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return nil, vmm.Process{}, errdefs.Context(err, operation, reference, "lock", "retry the operation", false)
	}
	defer func() {
		if unlockErr := lock.Unlock(context.WithoutCancel(ctx)); unlockErr != nil {
			backend = nil
			process = vmm.Process{}
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), operation, reference, "unlock", "retry the operation", false)
		}
	}()

	record, err = s.reader.Resolve(ctx, record.ID.String())
	if err != nil {
		return nil, vmm.Process{}, err
	}
	if record.State != types.SandboxStateRunning {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not running", record.ID, record.State))
	}
	if record.Generation < 2 {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("running sandbox has no Starting generation"))
	}
	backend, err = s.runtimes.Backend(record.VMM)
	if err != nil {
		return nil, vmm.Process{}, err
	}
	process, exists, err := backend.Locate(ctx, record.ID, record.Generation-1)
	if err != nil {
		return nil, vmm.Process{}, errdefs.Context(err, operation, reference, "locate VMM", "inspect the sandbox runtime", false)
	}
	if !exists {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("sandbox state is running but its VMM process is absent"))
	}
	return backend, process, nil
}

// Remove records cleanup intent before deleting the COW directory and releases
// the name and image reference only after filesystem cleanup succeeds.
//
//	resolve -> sandbox lock -> Deleting -> remove files -> forget record + name
//	                              |                            |
//	                              +---- retry resumes here <---+
func (s *SandboxService) Remove(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.reader == nil || s.remover == nil || s.disks == nil || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.reader.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "remove sandbox", reference, "lock", "retry the removal", false)
	}
	committed := false
	defer func() {
		unlockErr := lock.Unlock(context.WithoutCancel(ctx))
		if unlockErr != nil {
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "remove sandbox", reference, "unlock", "inspect the sandbox removal state before retrying", committed)
		}
	}()
	if err := s.reporter.Status("marking sandbox for deletion"); err != nil {
		return types.Sandbox{}, err
	}
	deleting, err := s.remover.BeginDelete(ctx, record.ID, record.Generation, s.now().UTC())
	if err != nil {
		return types.Sandbox{}, err
	}
	committed = true
	result = deleting
	if err := s.reporter.Status("removing sandbox disk"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.disks.Remove(ctx, deleting.ID); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "disk cleanup", "retry removal to finish cleanup", true)
	}
	if err := s.reporter.Status("releasing metadata and image reference"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.remover.FinalizeDelete(ctx, deleting.ID, deleting.Generation); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "finalize", "retry removal to finish cleanup", true)
	}
	if err := s.reporter.Committed(deleting); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "sandbox was deleted; do not retry", true)
	}
	return deleting, nil
}

// compensate removes the owned disk before forgetting the Creating reservation.
// If cleanup cannot be proven complete, Error retains the resource owner and image pin.
func (s *SandboxService) compensate(ctx context.Context, record types.Sandbox, phase string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	removeErr := s.disks.Remove(cleanupCtx, record.ID)
	if removeErr == nil {
		forgetErr := s.creator.Forget(cleanupCtx, record.ID, record.Generation)
		if forgetErr == nil {
			return errdefs.Context(cause, "create sandbox", record.Config.Name, phase, "fix the failure and retry", false)
		}
		removeErr = forgetErr
	}
	failure := types.SandboxFailure{Phase: phase, Message: errors.Join(cause, removeErr).Error()}
	_, markErr := s.creator.MarkError(cleanupCtx, record.ID, record.Generation, failure, s.now().UTC())
	return errdefs.Context(errors.Join(cause, removeErr, markErr), "create sandbox", record.Config.Name, phase, "inspect or remove the retained error sandbox", false)
}

// discardReporter keeps reporting optional for non-CLI consumers.
type discardReporter struct{}

func (discardReporter) Status(string) error           { return nil }
func (discardReporter) Committed(types.Sandbox) error { return nil }
