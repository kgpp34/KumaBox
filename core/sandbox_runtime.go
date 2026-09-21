package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// Start validates persistent inputs, recovers an interrupted launch when
// possible, and commits Running only after the exact VMM reports readiness.
//
//	resolve + lock -> verify image/COW -> Starting -> launch -> API Running
//	                        ^               |                    |
//	                        +---- retry ----+-------- CAS Running+
//	                                        |
//	                              abort + retained Error
func (s *SandboxService) Start(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.images == nil || s.dependencies.disks == nil || s.dependencies.runtimes.Len() == 0 || s.dependencies.reporter == nil || s.dependencies.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.dependencies.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.dependencies.reporter.Status("waiting for sandbox operation lock"); err != nil {
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
	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
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
		if err := s.dependencies.reporter.Committed(result); err != nil {
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

	if err := s.dependencies.reporter.Status("checking host runtime"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	if err := backend.Preflight(); err != nil {
		return record, failBeforeLaunch("host preflight", err)
	}
	if int(record.Config.CPUs) > runtime.NumCPU() {
		return record, failBeforeLaunch("host capacity", errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("requested %d vCPUs exceeds available host CPUs (%d)", record.Config.CPUs, runtime.NumCPU())))
	}
	if err := s.dependencies.reporter.Status("verifying image and sandbox disk"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	var plan vmm.LaunchPlan
	_, err = s.dependencies.images.WithAvailable(ctx, record.ImageDigest.String(), func(image types.Image) error {
		var buildErr error
		plan, buildErr = s.launchPlan(record, image)
		if buildErr != nil {
			return buildErr
		}
		return s.dependencies.disks.Check(ctx, record.ID, record.Config.Storage)
	})
	if err != nil {
		return record, failBeforeLaunch("validate artifacts", err)
	}

	if err := s.dependencies.reporter.Status("committing starting state"); err != nil {
		return record, failBeforeLaunch("report", err)
	}
	starting, err := s.dependencies.catalog.BeginStart(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
	if err != nil {
		return record, errdefs.Context(err, "start sandbox", reference, "mark starting", "inspect the sandbox before retrying", committed)
	}
	committed = true
	result = starting
	plan.Generation = starting.Generation
	if err := plan.Validate(); err != nil {
		return starting, s.failStart(ctx, backend, starting, "build launch plan", err, vmm.Process{})
	}
	if err := s.dependencies.reporter.Status("launching " + string(backend.Type())); err != nil {
		return starting, s.failStart(ctx, backend, starting, "report", err, vmm.Process{})
	}
	process, err := backend.Launch(ctx, plan)
	if err != nil {
		return starting, s.failStart(ctx, backend, starting, "launch VMM", err, process)
	}
	if err := s.dependencies.reporter.Status("committing running state"); err != nil {
		return starting, s.failStart(ctx, backend, starting, "report", err, process)
	}
	running, err := s.dependencies.catalog.MarkRunning(ctx, starting.ID, starting.Generation, s.dependencies.now().UTC())
	if err != nil {
		return starting, s.failStart(ctx, backend, starting, "commit running", err, process)
	}
	result = running
	if err := s.dependencies.reporter.Committed(running); err != nil {
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
	if err := s.dependencies.reporter.Status("checking existing runtime"); err != nil {
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
			stopped, err := s.dependencies.catalog.MarkStopped(ctx, record.ID, record.Generation, types.SandboxStateRunning, s.dependencies.now().UTC())
			return stopped, false, err
		}
	case types.SandboxStateStarting:
		switch observation.State {
		case vmm.ProcessRunning:
			running, err := s.dependencies.catalog.MarkRunning(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
			return running, err == nil, err
		case vmm.ProcessStarting:
			if err := backend.WaitReady(ctx, observation.Process); err != nil {
				return record, false, s.failStart(ctx, backend, record, "recover VMM", err, observation.Process)
			}
			running, err := s.dependencies.catalog.MarkRunning(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
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
	kernel, err := s.dependencies.imagePaths.BootFile(image.Boot.KernelLayer, image.Boot.KernelFile)
	if err != nil {
		return vmm.LaunchPlan{}, err
	}
	initrd, err := s.dependencies.imagePaths.BootFile(image.Boot.InitrdLayer, image.Boot.InitrdFile)
	if err != nil {
		return vmm.LaunchPlan{}, err
	}
	cmdline, err := vmm.OverlayV1Cmdline(vmm.OverlayV1Config{LayerCount: len(image.Layers), Hostname: record.Config.Name})
	if err != nil {
		return vmm.LaunchPlan{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeImageIncompatible, err)
	}
	disks := make([]vmm.Disk, 0, len(image.Layers)+1)
	for position, layer := range image.Layers {
		disks = append(disks, vmm.Disk{Path: s.dependencies.imagePaths.EROFS(layer.SourceDigest), Serial: fmt.Sprintf("%s%d", vmm.LayerSerialPrefix, position), ReadOnly: true})
	}
	cow, err := s.dependencies.paths.COW(record.ID)
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
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.dependencies.cleanupTimeout)
	defer cancel()
	var cleanupErr error
	if process.PID > 0 {
		cleanupErr = backend.Abort(cleanupCtx, process)
	} else {
		cleanupErr = backend.Cleanup(cleanupCtx, starting.ID)
	}
	failureCause := errors.Join(cause, cleanupErr)
	failure := types.SandboxFailure{Phase: phase, Message: failureCause.Error()}
	_, markErr := s.dependencies.catalog.MarkStartError(cleanupCtx, starting.ID, starting.Generation, failure, s.dependencies.now().UTC())
	return errdefs.Context(errors.Join(failureCause, markErr), "start sandbox", starting.Config.Name, phase, "inspect the retained error sandbox and VMM log", true)
}

// Stop terminates the exact VMM process owned by one sandbox and commits
// Stopped only after process absence and runtime cleanup are proven.
//
//	Running + live VMM -> Stopping -> TERM -> grace -> KILL -> cleanup -> Stopped
//	Starting/Stopping  ----- retry resumes the owned process generation -----^
//	Running + no VMM  --------------------- cleanup ------------------------^
func (s *SandboxService) Stop(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 || s.dependencies.reporter == nil || s.dependencies.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.dependencies.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.dependencies.reporter.Status("waiting for sandbox operation lock"); err != nil {
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
	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	result = record
	if record.State == types.SandboxStateCreating || record.State == types.SandboxStateDeleting {
		return record, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s in state %s cannot stop", record.ID, record.State))
	}
	if record.State == types.SandboxStateCreated || record.State == types.SandboxStateStopped {
		if err := s.dependencies.reporter.Status("cleaning stale runtime state"); err != nil {
			return record, err
		}
		if err := backend.Cleanup(ctx, record.ID); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "cleanup runtime", "inspect the runtime scope before retrying", false)
		}
		if err := s.dependencies.reporter.Committed(record); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "sandbox is not running", false)
		}
		return record, nil
	}

	processGeneration, err := stopProcessGeneration(record)
	if err != nil {
		return record, err
	}
	if err := s.dependencies.reporter.Status("checking existing runtime"); err != nil {
		return record, err
	}
	process, exists, err := backend.Locate(ctx, record.ID, processGeneration)
	if err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "observe runtime", "inspect the sandbox runtime before retrying", false)
	}

	if record.State == types.SandboxStateRunning && exists {
		if err := s.dependencies.reporter.Status("committing stopping state"); err != nil {
			return record, err
		}
		record, err = s.dependencies.catalog.BeginStop(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
		if err != nil {
			return result, errdefs.Context(err, "stop sandbox", reference, "mark stopping", "inspect the sandbox before retrying", false)
		}
		result, committed = record, true
	}

	if exists {
		if err := s.dependencies.reporter.Status("stopping " + string(backend.Type())); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to resume Stopping", committed)
		}
		if err := backend.Stop(ctx, process); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "stop VMM", "retry the stop; the retained state preserves ownership", committed)
		}
	}
	if err := s.dependencies.reporter.Status("cleaning runtime state"); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to finish cleanup", committed)
	}
	if err := backend.Cleanup(ctx, record.ID); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "cleanup runtime", "retry the stop to finish cleanup", committed)
	}

	// Error retains the original start/create diagnostic after any residual VMM
	// is gone. It can be removed or started explicitly by the next command.
	if record.State == types.SandboxStateError {
		if err := s.dependencies.reporter.Committed(record); err != nil {
			return record, errdefs.Context(err, "stop sandbox", reference, "report", "the VMM is stopped; inspect the retained error", committed)
		}
		return record, nil
	}
	if err := s.dependencies.reporter.Status("committing stopped state"); err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "report", "retry the stop to commit process absence", committed)
	}
	stopped, err := s.dependencies.catalog.MarkStopped(ctx, record.ID, record.Generation, record.State, s.dependencies.now().UTC())
	if err != nil {
		return record, errdefs.Context(err, "stop sandbox", reference, "mark stopped", "inspect the sandbox before retrying", committed)
	}
	result, committed = stopped, true
	if err := s.dependencies.reporter.Committed(stopped); err != nil {
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

// SandboxLogOptions contains application-level log selection without exposing
// a concrete backend's filesystem layout to the CLI.
type SandboxLogOptions struct {
	// Tail starts output at the last N lines. Zero selects the complete log.
	Tail int
	// Follow keeps the stream open for appended output until cancellation.
	Follow bool
}

// Logs streams persistent VMM output for any retained sandbox state. It does
// not hold the entity lock while following, so start, stop, and rm can progress.
func (s *SandboxService) Logs(ctx context.Context, reference string, options SandboxLogOptions, output io.Writer) error {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	backendOptions := vmm.LogOptions{Tail: options.Tail, Follow: options.Follow}
	if err := backendOptions.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if output == nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("log output is required"))
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return err
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return err
	}
	if err := backend.Logs(ctx, record.ID, backendOptions, output); err != nil {
		return errdefs.Context(err, "read sandbox logs", reference, "stream VMM log", "start the sandbox if it has no log, or retry the stream", false)
	}
	return nil
}

// Exec runs one command through the guest agent after resolving an exact live
// VMM process. The operation lock is released before network I/O and command
// execution so stop can always make progress.
//
//	resolve + lock -> Running generation -> locate process -> unlock
//	                                                        |
//	                          vsock -> agent stream -> exit code
func (s *SandboxService) Exec(ctx context.Context, reference string, command types.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := command.Validate(); err != nil {
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
	exitCode, err := agent.Run(ctx, connection, command, stdin, stdout, stderr)
	if err != nil {
		return 0, errdefs.Context(err, "execute sandbox command", reference, "run guest command", "inspect the guest agent and retry", false)
	}
	return exitCode, nil
}

// locateRunning returns an identity-checked VMM generation. It holds the
// sandbox operation lock only while persistent and process facts are resolved.
func (s *SandboxService) locateRunning(ctx context.Context, reference, operation string) (backend vmm.Backend, process vmm.Process, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return nil, vmm.Process{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return nil, vmm.Process{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
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

	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return nil, vmm.Process{}, err
	}
	if record.State != types.SandboxStateRunning {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not running", record.ID, record.State))
	}
	if record.Generation < 2 {
		return nil, vmm.Process{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("running sandbox has no Starting generation"))
	}
	backend, err = s.dependencies.runtimes.Backend(record.VMM)
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
