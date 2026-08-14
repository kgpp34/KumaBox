package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/disk"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/metering"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vm"
	"golang.org/x/sync/errgroup"
)

// NativeRestoreOptions controls in-place restoration of a running snapshot.
type NativeRestoreOptions struct {
	Mode RestoreMode
}

type stagedRestore struct {
	nativeDir      string
	preserveNative bool
	disks          []stagedRestoreDisk
}

type stagedRestoreDisk struct {
	id     string
	target string
	staged string
}

type restoreStageMetrics struct {
	nativeStageDuration time.Duration
	diskStageDuration   time.Duration
}

// RestoreNativeVM restores native memory, device state, and writable disks
// into the original VM identity. Snapshot and VM operation locks are held for
// the complete transaction.
func (r *Runtime) RestoreNativeVM(ctx context.Context, vmRef, snapshotRef string, opts NativeRestoreOptions) (result *vm.VMRecord, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	mode, err := normalizeRestoreMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	opts.Mode = mode
	restoreStarted := time.Now()
	rec, err := r.vmReader.Inspect(vmRef)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperationWithRelated(ctx, operation.KindSnapshotRestoreVM, rec.ID, snapshotRef)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for restore: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck

	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	if rec.State != vm.StateRunning && rec.State != vm.StateStopped && rec.State != vm.StateError {
		return nil, fmt.Errorf("VM_RESTORE_INVALID_STATE: VM %s is %s", rec.Name, rec.State)
	}
	restorer, ok := r.backend.(backend.NativeRestorer)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not support native restore")
	}
	inspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}

	snapshotStore := r.data.Snapshots
	snapshotRec, lease, err := snapshotStore.AcquireRead(ctx, snapshotRef)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck
	host, err := inspector.InspectNativeHost(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	if err := requireRestoreMode(host, opts.Mode); err != nil {
		return nil, err
	}
	manifest, err := snapshotStore.VerifyNativeRecord(ctx, snapshotRec, snapshot.NativeVerifyTarget{VM: rec, Host: host})
	if err != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", err)
	}
	if manifest.Network == nil || manifest.Network.RestorePolicy != "preserve" {
		return nil, errors.New("SNAPSHOT_INCOMPATIBLE: snapshot does not preserve VM network identity")
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		return nil, fmt.Errorf("render restore launch config: %w", err)
	}
	staged, stageMetrics, err := stageNativeRestore(ctx, snapshotRec, manifest, rec)
	if err != nil {
		return nil, err
	}
	defer staged.cleanup() //nolint:errcheck

	observed := r.applyObservation(rec)
	if observed.ObservedState == vm.ObservedStateRunning || observed.ObservedState == vm.ObservedStatePaused {
		if _, err := r.stopVMLocked(ctx, rec.ID, backend.StopOptions{Force: true, Timeout: forcedStopTimeout}, metering.ReasonRestore); err != nil {
			return nil, fmt.Errorf("stop VM for restore: %w", err)
		}
	}
	dirty, err := r.vmRestore.BeginRestore(rec.ID, snapshotRec.ID, string(opts.Mode))
	if err != nil {
		return nil, fmt.Errorf("mark restore dirty: %w", err)
	}
	fail := func(cause error) (*vm.VMRecord, error) {
		_, markErr := r.vmRestore.FailRestore(rec.ID, cause.Error())
		return nil, errors.Join(cause, markErr)
	}
	diskCommitStarted := time.Now()
	if err := staged.commitDisks(); err != nil {
		return fail(fmt.Errorf("replace writable disks: %w", err))
	}
	diskCommitDuration := time.Since(diskCommitStarted)
	backendRestoreStarted := time.Now()
	backendResult, err := restorer.RestoreVM(ctx, dirty, staged.nativeDir, string(opts.Mode))
	if err != nil {
		return fail(fmt.Errorf("restore backend state: %w", err))
	}
	backendRestoreDuration := time.Since(backendRestoreStarted)
	stopRestoredBackend := func(cause error) (*vm.VMRecord, error) {
		cleanupRec := *dirty
		cleanupRec.PID = backendResult.PID
		cleanupRec.APISocket = backendResult.APISocket
		_, stopErr := r.backend.StopVM(&cleanupRec, backend.StopOptions{Force: true})
		if stopErr != nil && restoreModePinsSnapshot(opts.Mode) {
			staged.retainNativePayload()
		}
		return fail(errors.Join(cause, stopErr))
	}
	identityStarted := time.Now()
	reseedErr := reseedRestoredGuest(ctx, rec.VsockSocket, false)
	identityDuration := time.Since(identityStarted)
	if err := r.recordVMSnapshotReference(ctx, dirty.ID, snapshotRec.ID); err != nil {
		return stopRestoredBackend(fmt.Errorf("record restore snapshot reference: %w", err))
	}
	// Backend restore is the lifecycle boundary. Agent-dependent commands
	// report their own availability without quarantining this running VM.
	restored, err := r.vmRestore.CompleteRestore(rec.ID, backendResult.PID, backendResult.APISocket, time.Since(restoreStarted), &vm.RestoreResult{
		NativeStageDurationMs:    stageMetrics.nativeStageDuration.Milliseconds(),
		DiskStageDurationMs:      stageMetrics.diskStageDuration.Milliseconds(),
		DiskCommitDurationMs:     diskCommitDuration.Milliseconds(),
		BackendRestoreDurationMs: backendRestoreDuration.Milliseconds(),
		IdentityDurationMs:       identityDuration.Milliseconds(),
		GuestAgentWarning:        guestAgentWarning(reseedErr),
	})
	if err != nil {
		removeErr := r.removeVMSnapshotReference(ctx, dirty.ID, snapshotRec.ID)
		return stopRestoredBackend(errors.Join(fmt.Errorf("publish restored VM state: %w", err), removeErr))
	}
	r.recordComputeStart(ctx, restored, metering.ReasonRestore)
	if restoreModePinsSnapshot(opts.Mode) {
		staged.retainNativePayload()
	}
	_ = writeVMEvent(restored, "snapshot.restore.completed", vm.Observation{
		State: vm.ObservedStateRunning, Reason: "native snapshot " + snapshotRec.ID + " restored", CheckedAt: time.Now().UTC(),
	})
	return r.applyObservation(restored), nil
}

func stageNativeRestore(ctx context.Context, snapshotRec *snapshot.Record, manifest *snapshot.Manifest, rec *vm.VMRecord) (*stagedRestore, restoreStageMetrics, error) {
	var metrics restoreStageMetrics
	nativeStageStarted := time.Now()
	root := filepath.Join(rec.RunDir, ".restore-staging")
	if err := os.RemoveAll(root); err != nil {
		return nil, metrics, fmt.Errorf("clear restore staging: %w", err)
	}
	nativeDir := filepath.Join(root, snapshot.NativePayloadDir)
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, metrics, fmt.Errorf("create native restore staging: %w", err)
	}
	staged := &stagedRestore{nativeDir: nativeDir}
	ok := false
	defer func() {
		if !ok {
			_ = staged.cleanup()
		}
	}()
	for _, file := range manifest.Native.Files {
		if !strings.HasPrefix(file.Path, snapshot.NativePathPrefix) || filepath.Base(file.Path) != strings.TrimPrefix(file.Path, snapshot.NativePathPrefix) {
			return nil, metrics, fmt.Errorf("SNAPSHOT_CORRUPT: invalid native payload path %s", file.Path)
		}
		source := filepath.Join(snapshotRec.DataDir, filepath.FromSlash(file.Path))
		destination := filepath.Join(nativeDir, filepath.Base(file.Path))
		// Cloud Hypervisor owns eager-copy versus delayed paging. The host must
		// not make a second full copy before vm.restore in either case.
		if snapshot.IsNativeMemoryFile(file.Path) {
			if err := linkNativeMemory(source, destination); err != nil {
				return nil, metrics, fmt.Errorf("link native memory payload %s: %w", file.Path, err)
			}
			continue
		}
		result, err := disk.CopyFile(ctx, source, destination)
		if err != nil {
			return nil, metrics, fmt.Errorf("stage native payload %s: %w", file.Path, err)
		}
		if file.SHA256 != "" && result.SHA256 != file.SHA256 {
			return nil, metrics, fmt.Errorf("CHECKSUM_MISMATCH: staged %s", file.Path)
		}
	}
	metrics.nativeStageDuration = time.Since(nativeStageStarted)
	diskStageStarted := time.Now()
	targets := make(map[string]vm.StorageConfig, len(rec.StorageConfigs))
	for _, disk := range rec.StorageConfigs {
		if disk.EffectiveRole() == vm.StorageRoleCOW || disk.EffectiveRole() == vm.StorageRoleData {
			targets[disk.ID] = disk
		}
	}
	staged.disks = make([]stagedRestoreDisk, len(manifest.Disks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(disk.MaxConcurrentFileCopies)
	for index, manifestDisk := range manifest.Disks {
		index, manifestDisk := index, manifestDisk
		group.Go(func() error {
			target, found := targets[manifestDisk.ID]
			if !found {
				return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: no writable target for disk %s", manifestDisk.ID)
			}
			if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
				return fmt.Errorf("create target directory for disk %s: %w", manifestDisk.ID, err)
			}
			stagedPath := filepath.Join(filepath.Dir(target.Path), ".kumabox-restore-"+snapshotRec.ID+"-"+filepath.Base(target.Path))
			if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("clear staged disk %s: %w", manifestDisk.ID, err)
			}
			source := filepath.Join(snapshotRec.DataDir, filepath.FromSlash(manifestDisk.Path))
			result, err := disk.CopyFile(groupCtx, source, stagedPath)
			if err != nil {
				return fmt.Errorf("stage writable disk %s: %w", manifestDisk.ID, err)
			}
			if manifestDisk.SHA256 != "" && result.SHA256 != manifestDisk.SHA256 {
				return fmt.Errorf("CHECKSUM_MISMATCH: staged disk %s", manifestDisk.ID)
			}
			staged.disks[index] = stagedRestoreDisk{id: manifestDisk.ID, target: target.Path, staged: stagedPath}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, metrics, err
	}
	if len(staged.disks) != len(targets) {
		return nil, metrics, errors.New("SNAPSHOT_INCOMPATIBLE: writable disk set is incomplete")
	}
	metrics.diskStageDuration = time.Since(diskStageStarted)
	ok = true
	return staged, metrics, nil
}

func linkNativeMemory(source, destination string) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := os.Symlink(source, destination); err != nil {
		return fmt.Errorf("cross-filesystem symlink: %w", err)
	}
	return nil
}

func (s *stagedRestore) commitDisks() error {
	for _, disk := range s.disks {
		if err := os.Rename(disk.staged, disk.target); err != nil {
			return fmt.Errorf("replace disk %s: %w", disk.id, err)
		}
		if err := syncDirectory(filepath.Dir(disk.target)); err != nil {
			return fmt.Errorf("sync disk %s directory: %w", disk.id, err)
		}
	}
	return nil
}

func (s *stagedRestore) cleanup() error {
	if s == nil {
		return nil
	}
	var errs []error
	if !s.preserveNative {
		errs = append(errs, os.RemoveAll(filepath.Dir(s.nativeDir)))
	}
	for _, disk := range s.disks {
		if err := os.Remove(disk.staged); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *stagedRestore) retainNativePayload() {
	if s != nil {
		s.preserveNative = true
	}
}

func syncDirectory(path string) (err error) {
	dir, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer fileutil.CloseAndJoin(&err, dir, "close restore directory")
	return dir.Sync()
}
