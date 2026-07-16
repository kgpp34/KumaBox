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

const snapshotCleanupTimeout = 30 * time.Second

var freezeSnapshotFilesystems = kbagent.FreezeFilesystems
var thawSnapshotFilesystems = kbagent.ThawFilesystems

type RunningSnapshotOptions struct {
	Consistency string
}

// CreateRunningSnapshot captures native backend state and writable disks from
// one pause window, then publishes the snapshot after the source VM resumes.
func (r *Runtime) CreateRunningSnapshot(ctx context.Context, ref, name string) (*snapshot.Record, error) {
	return r.CreateRunningSnapshotWithOptions(ctx, ref, name, RunningSnapshotOptions{Consistency: "crash"})
}

func (r *Runtime) CreateRunningSnapshotWithOptions(ctx context.Context, ref, name string, opts RunningSnapshotOptions) (*snapshot.Record, error) {
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
		return nil, fmt.Errorf("lock VM %s for running snapshot: %w", rec.ID, err)
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
	hostInspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}

	build, err := snapshot.NewStore(r.store.RootDir()).Reserve(ctx, name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	pending := build.Record()
	nativeDir := filepath.Join(pending.StagingDir, "native")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create native snapshot staging: %w", err)
	}

	frozen := false
	if opts.Consistency == "fs" {
		freezeCtx, cancel := context.WithTimeout(ctx, snapshotCleanupTimeout)
		_, freezeErr := freezeSnapshotFilesystems(freezeCtx, rec.VsockSocket)
		cancel()
		if freezeErr != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
			_, thawErr := thawSnapshotFilesystems(cleanupCtx, rec.VsockSocket)
			cleanupCancel()
			if errors.Is(freezeErr, kbagent.ErrNotReady) {
				return nil, errors.Join(fmt.Errorf("GUEST_AGENT_UNAVAILABLE: freeze filesystems: %w", freezeErr), wrapOptional("cleanup thaw", thawErr))
			}
			return nil, errors.Join(fmt.Errorf("GUEST_FREEZE_FAILED: %w", freezeErr), wrapOptional("cleanup thaw", thawErr))
		}
		frozen = true
	}
	if err := controller.PauseVM(ctx, rec); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
		thawErr := r.thawSnapshotGuest(cleanupCtx, rec, frozen)
		cancel()
		return nil, errors.Join(fmt.Errorf("pause VM for snapshot: %w", err), thawErr)
	}
	stagedDisks, captureErr := captureNativeWindow(ctx, snapshotter, rec, nativeDir, pending.StagingDir)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	resumeErr := controller.ResumeVM(cleanupCtx, rec)
	thawErr := r.thawSnapshotGuest(cleanupCtx, rec, frozen)
	cancel()
	if captureErr != nil || resumeErr != nil || thawErr != nil {
		if resumeErr != nil {
			r.persistSnapshotResumeFailure(rec)
		}
		return nil, errors.Join(
			wrapOptional("capture native snapshot", captureErr),
			wrapOptional("resume VM after snapshot", resumeErr),
			wrapOptional("thaw guest filesystems", thawErr),
		)
	}
	disks, _, err := snapshot.FinalizeWritableDisks(ctx, pending.StagingDir, stagedDisks)
	if err != nil {
		return nil, fmt.Errorf("finalize writable disks: %w", err)
	}

	host, err := hostInspector.InspectNativeHost(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	manifest, totalSize, err := snapshot.WriteNativeManifest(ctx, build, rec, disks, host, opts.Consistency)
	if err != nil {
		return nil, err
	}
	ready, err := build.Finalize(totalSize)
	if err != nil {
		return nil, err
	}
	_ = writeVMEvent(rec, "snapshot.capture.completed", vmstore.Observation{
		State:     vmstore.ObservedStateRunning,
		Reason:    fmt.Sprintf("native snapshot %s captured with %s consistency", ready.ID, manifest.Consistency),
		CheckedAt: time.Now().UTC(),
	})
	return ready, nil
}

func (r *Runtime) thawSnapshotGuest(ctx context.Context, rec *vmstore.VMRecord, frozen bool) error {
	if !frozen {
		return nil
	}
	_, err := thawSnapshotFilesystems(ctx, rec.VsockSocket)
	return err
}

func captureNativeWindow(ctx context.Context, snapshotter backend.NativeSnapshotter, rec *vmstore.VMRecord, nativeDir, stagingDir string) ([]snapshot.DiskManifest, error) {
	if err := snapshotter.SnapshotVM(ctx, rec, nativeDir); err != nil {
		return nil, fmt.Errorf("capture backend state: %w", err)
	}
	disks, err := snapshot.StageWritableDisks(ctx, stagingDir, rec)
	if err != nil {
		return nil, fmt.Errorf("capture writable disks: %w", err)
	}
	return disks, nil
}

func (r *Runtime) persistSnapshotResumeFailure(rec *vmstore.VMRecord) {
	observation := r.backend.ObserveVM(rec)
	if observation.State == vmstore.ObservedStatePaused {
		_, _ = r.store.MarkPaused(rec.ID)
		return
	}
	_, _ = r.store.MarkError(rec.ID, "failed to resume VM after running snapshot")
}

func wrapOptional(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
