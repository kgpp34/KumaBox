package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	kbagent "github.com/kumabox/kumabox/internal/agent"
	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type HibernateOptions struct {
	Name        string
	Consistency string
}

type HibernateResult struct {
	VM       *vmstore.VMRecord `json:"vm"`
	Snapshot *snapshot.Record  `json:"snapshot"`
}

// HibernateVM durably captures a paused VM and terminates the VMM without a
// resume gap. Persistence failure resumes the original process.
func (r *Runtime) HibernateVM(ctx context.Context, ref string, opts HibernateOptions) (*HibernateResult, error) {
	if opts.Name == "" {
		return nil, errors.New("hibernate snapshot name must not be empty")
	}
	if opts.Consistency == "" {
		opts.Consistency = "crash"
	}
	if opts.Consistency != "crash" && opts.Consistency != "fs" {
		return nil, fmt.Errorf("SNAPSHOT_CONSISTENCY_UNSUPPORTED: %s", opts.Consistency)
	}
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for hibernate: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.store.Inspect(rec.ID)
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

	snapshotStore := snapshot.NewStore(r.store.RootDir())
	build, err := snapshotStore.Reserve(ctx, opts.Name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	pending := build.Record()
	nativeDir := filepath.Join(pending.StagingDir, "native")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create hibernate staging: %w", err)
	}

	frozen, err := r.freezeForHibernate(ctx, rec, opts.Consistency)
	if err != nil {
		return nil, err
	}
	if err := controller.PauseVM(ctx, rec); err != nil {
		return nil, errors.Join(fmt.Errorf("pause VM for hibernate: %w", err), r.recoverHibernateGuest(ctx, controller, rec, false, frozen))
	}

	ready, persistErr := r.persistHibernationSnapshot(ctx, build, rec, snapshotter, inspector, nativeDir, opts.Consistency)
	if persistErr != nil {
		return nil, errors.Join(persistErr, r.recoverHibernateGuest(ctx, controller, rec, true, frozen))
	}
	if _, err := r.backend.StopVM(rec, backend.StopOptions{Force: true, Timeout: 5 * time.Second}); err != nil {
		recoverErr := r.recoverHibernateGuest(ctx, controller, rec, true, frozen)
		removeErr := error(nil)
		if recoverErr == nil {
			_, removeErr = snapshotStore.Remove(ready.ID)
		}
		return nil, errors.Join(fmt.Errorf("terminate hibernated VMM: %w", err), recoverErr, removeErr)
	}
	hibernated, err := r.store.MarkHibernated(rec.ID, ready.ID)
	if err != nil {
		_, _ = r.store.MarkError(rec.ID, "hibernate snapshot is durable but stopped state publication failed")
		return nil, fmt.Errorf("publish hibernated VM state: %w", err)
	}
	_ = writeVMEvent(hibernated, "vm.hibernate.completed", vmstore.Observation{
		State: vmstore.ObservedStateStopped, Reason: "hibernated to native snapshot " + ready.ID, CheckedAt: time.Now().UTC(),
	})
	return &HibernateResult{VM: r.applyObservation(hibernated), Snapshot: ready}, nil
}

func (r *Runtime) freezeForHibernate(ctx context.Context, rec *vmstore.VMRecord, consistency string) (bool, error) {
	if consistency != "fs" {
		return false, nil
	}
	freezeCtx, cancel := context.WithTimeout(ctx, snapshotCleanupTimeout)
	_, err := freezeSnapshotFilesystems(freezeCtx, rec.VsockSocket)
	cancel()
	if err == nil {
		return true, nil
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	_, thawErr := thawSnapshotFilesystems(cleanupCtx, rec.VsockSocket)
	cleanupCancel()
	if errors.Is(err, kbagent.ErrNotReady) {
		return false, errors.Join(fmt.Errorf("GUEST_AGENT_UNAVAILABLE: freeze filesystems: %w", err), thawErr)
	}
	return false, errors.Join(fmt.Errorf("GUEST_FREEZE_FAILED: %w", err), thawErr)
}

func (r *Runtime) persistHibernationSnapshot(ctx context.Context, build *snapshot.Build, rec *vmstore.VMRecord, snapshotter backend.NativeSnapshotter, inspector backend.NativeHostInspector, nativeDir, consistency string) (*snapshot.Record, error) {
	pending := build.Record()
	stagedDisks, err := captureNativeWindow(ctx, snapshotter, rec, nativeDir, pending.StagingDir)
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
	_, totalSize, err := snapshot.WriteNativeManifest(ctx, build, rec, disks, host, consistency)
	if err != nil {
		return nil, err
	}
	ready, err := build.Finalize(totalSize)
	if err != nil {
		return nil, err
	}
	return ready, nil
}

func (r *Runtime) recoverHibernateGuest(ctx context.Context, controller backend.StateController, rec *vmstore.VMRecord, paused, frozen bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	errs := make([]error, 0, 2)
	if paused {
		if err := controller.ResumeVM(cleanupCtx, rec); err != nil {
			r.persistSnapshotResumeFailure(rec)
			errs = append(errs, fmt.Errorf("resume VM after failed hibernate: %w", err))
		}
	}
	if frozen {
		if _, err := thawSnapshotFilesystems(cleanupCtx, rec.VsockSocket); err != nil {
			errs = append(errs, fmt.Errorf("thaw VM after failed hibernate: %w", err))
		}
	}
	return errors.Join(errs...)
}
