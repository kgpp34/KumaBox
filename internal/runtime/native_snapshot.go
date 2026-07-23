package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const snapshotCleanupTimeout = 30 * time.Second

// CreateRunningSnapshot captures native backend state and writable disks from
// one pause window, then publishes the snapshot after the source VM resumes.
func (r *Runtime) CreateRunningSnapshot(ctx context.Context, ref, name string) (*snapshot.Record, error) {
	captureStarted := time.Now()
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for running snapshot: %w", rec.ID, err)
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
	hostInspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}

	build, err := r.storeSet.Snapshots.Reserve(ctx, name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	pending := build.Record()
	nativeDir := filepath.Join(pending.StagingDir, snapshot.NativePayloadDir)
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create native snapshot staging: %w", err)
	}

	if err := controller.PauseVM(ctx, rec); err != nil {
		return nil, fmt.Errorf("pause VM for snapshot: %w", err)
	}
	pausedAt := time.Now()
	stagedDisks, nativeCaptureMs, diskStageMs, captureErr := captureNativeWindow(ctx, snapshotter, rec, nativeDir, pending.StagingDir)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	resumeErr := controller.ResumeVM(cleanupCtx, rec)
	resumedAt := time.Now()
	cancel()
	if captureErr != nil || resumeErr != nil {
		if resumeErr != nil {
			r.persistSnapshotResumeFailure(rec)
		}
		return nil, errors.Join(
			wrapOptional("capture native snapshot", captureErr),
			wrapOptional("resume VM after snapshot", resumeErr),
		)
	}
	// Running snapshots follow the fast local path: resume before durability
	// work. Strict fsync and hashing belong to explicit verification/export.
	disks := stagedDisks
	if err := build.SetPerformance(snapshot.CaptureMetrics{
		PauseDurationMs:       resumedAt.Sub(pausedAt).Milliseconds(),
		NativeCaptureMs:       nativeCaptureMs,
		WritableDiskStageMs:   diskStageMs,
		PublicationDurationMs: time.Since(resumedAt).Milliseconds(),
		TotalDurationMs:       time.Since(captureStarted).Milliseconds(),
	}); err != nil {
		return nil, err
	}

	host, err := hostInspector.InspectNativeHost(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	_, totalSize, err := snapshot.WriteNativeManifestFast(ctx, build, rec, disks, host)
	if err != nil {
		return nil, err
	}
	ready, err := build.Finalize(totalSize)
	if err != nil {
		return nil, err
	}
	_ = writeVMEvent(rec, "snapshot.capture.completed", vmstore.Observation{
		State:     vmstore.ObservedStateRunning,
		Reason:    fmt.Sprintf("native crash-consistent snapshot %s captured", ready.ID),
		CheckedAt: time.Now().UTC(),
	})
	return ready, nil
}

func captureNativeWindow(ctx context.Context, snapshotter backend.NativeSnapshotter, rec *vmstore.VMRecord, nativeDir, stagingDir string) ([]snapshot.DiskManifest, int64, int64, error) {
	nativeStarted := time.Now()
	if err := snapshotter.SnapshotVM(ctx, rec, nativeDir); err != nil {
		return nil, 0, 0, fmt.Errorf("capture backend state: %w", err)
	}
	nativeDuration := time.Since(nativeStarted).Milliseconds()
	diskStarted := time.Now()
	disks, err := snapshot.StageWritableDisks(ctx, stagingDir, rec)
	if err != nil {
		return nil, nativeDuration, time.Since(diskStarted).Milliseconds(), fmt.Errorf("capture writable disks: %w", err)
	}
	return disks, nativeDuration, time.Since(diskStarted).Milliseconds(), nil
}

func (r *Runtime) persistSnapshotResumeFailure(rec *vmstore.VMRecord) {
	observation := r.backend.ObserveVM(rec)
	if observation.State == vmstore.ObservedStatePaused {
		_ = r.vmUpdater.UpdateStates([]string{rec.ID}, vmstore.StatePaused)
		return
	}
	_, _ = r.vmUpdater.SetError(rec.ID, "failed to resume VM after running snapshot")
}

func wrapOptional(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
