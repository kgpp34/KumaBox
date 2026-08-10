package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type HibernateOptions struct {
	Name string
}

type HibernateResult struct {
	VM       *vmstore.VMRecord `json:"vm"`
	Snapshot *snapshot.Record  `json:"snapshot"`
}

// HibernateVM durably captures a paused VM and terminates the VMM without a
// resume gap. Persistence failure resumes the original process.
func (r *Runtime) HibernateVM(ctx context.Context, ref string, opts HibernateOptions) (result *HibernateResult, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	if opts.Name == "" {
		return nil, errors.New("hibernate snapshot name must not be empty")
	}
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperationWithRelated(ctx, operation.KindVMHibernate, rec.ID, opts.Name)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for hibernate: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	rec = r.applyObservation(rec)
	if rec.ObservedState != vmstore.ObservedStateRunning {
		return nil, fmt.Errorf("VM_NOT_RUNNING: VM %s observed state is %s", rec.Name, rec.ObservedState)
	}
	controller, ok := r.backend.(backend.StateController)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not support pause/resume")
	}
	snapshotter, ok := r.backend.(backend.NativeSnapshotter)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not support native snapshots")
	}
	inspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}

	snapshotStore := r.storeSet.Snapshots
	build, err := snapshotStore.Reserve(ctx, opts.Name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	pending := build.Record()
	nativeDir := filepath.Join(pending.StagingDir, snapshot.NativePayloadDir)
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create hibernate staging: %w", err)
	}

	if err := controller.PauseVM(ctx, rec); err != nil {
		return nil, fmt.Errorf("pause VM for hibernate: %w", err)
	}

	ready, persistErr := r.persistHibernationSnapshot(ctx, build, rec, snapshotter, inspector, nativeDir)
	if persistErr != nil {
		return nil, errors.Join(persistErr, r.recoverHibernateGuest(ctx, controller, rec, true))
	}
	if _, err := r.backend.StopVM(rec, backend.StopOptions{Force: true, Timeout: forcedStopTimeout}); err != nil {
		recoverErr := r.recoverHibernateGuest(ctx, controller, rec, true)
		removeErr := error(nil)
		if recoverErr == nil {
			_, removeErr = snapshotStore.Remove(ready.ID)
		}
		return nil, errors.Join(fmt.Errorf("terminate hibernated VMM: %w", err), recoverErr, removeErr)
	}
	hibernated, err := r.vmUpdater.CompleteHibernate(rec.ID, ready.ID)
	if err != nil {
		_, _ = r.vmUpdater.SetError(rec.ID, "hibernate snapshot is durable but stopped state publication failed")
		return nil, fmt.Errorf("publish hibernated VM state: %w", err)
	}
	if rec.Image != nil {
		if err := r.recordSnapshotImageReference(ctx, ready.ID, rec.Image.ID); err != nil {
			return nil, fmt.Errorf("record hibernate image reference: %w", err)
		}
	}
	if err := r.recordVMSnapshotReference(ctx, hibernated.ID, ready.ID); err != nil {
		return nil, fmt.Errorf("record hibernate snapshot reference: %w", err)
	}
	_ = writeVMEvent(hibernated, "vm.hibernate.completed", vmstore.Observation{
		State: vmstore.ObservedStateStopped, Reason: "hibernated to native snapshot " + ready.ID, CheckedAt: time.Now().UTC(),
	})
	return &HibernateResult{VM: r.applyObservation(hibernated), Snapshot: ready}, nil
}

func (r *Runtime) persistHibernationSnapshot(ctx context.Context, build *snapshot.Build, rec *vmstore.VMRecord, snapshotter backend.NativeSnapshotter, inspector backend.NativeHostInspector, nativeDir string) (*snapshot.Record, error) {
	pending := build.Record()
	stagedDisks, _, _, err := captureNativeWindow(ctx, snapshotter, rec, nativeDir, pending.StagingDir)
	if err != nil {
		return nil, err
	}
	disks, _, err := snapshot.FinalizeWritableDisks(ctx, pending.StagingDir, stagedDisks)
	if err != nil {
		return nil, fmt.Errorf("finalize hibernate disks: %w", err)
	}
	host, err := inspector.InspectNativeHost(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	_, totalSize, err := snapshot.WriteNativeManifest(ctx, build, rec, disks, host)
	if err != nil {
		return nil, err
	}
	ready, err := build.Finalize(totalSize)
	if err != nil {
		return nil, err
	}
	return ready, nil
}

func (r *Runtime) recoverHibernateGuest(ctx context.Context, controller backend.StateController, rec *vmstore.VMRecord, paused bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	var resumeErr error
	if paused {
		if err := controller.ResumeVM(cleanupCtx, rec); err != nil {
			r.persistSnapshotResumeFailure(rec)
			resumeErr = fmt.Errorf("resume VM after failed hibernate: %w", err)
		}
	}
	return resumeErr
}
