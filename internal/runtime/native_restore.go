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
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// NativeRestoreOptions controls in-place restoration of a running snapshot.
type NativeRestoreOptions struct {
	Mode string
}

type stagedRestore struct {
	nativeDir string
	disks     []stagedRestoreDisk
}

type stagedRestoreDisk struct {
	id     string
	target string
	staged string
}

// RestoreNativeVM restores native memory, device state, and writable disks
// into the original VM identity. Snapshot and VM operation locks are held for
// the complete transaction.
func (r *Runtime) RestoreNativeVM(ctx context.Context, vmRef, snapshotRef string, opts NativeRestoreOptions) (*vmstore.VMRecord, error) {
	mode, err := normalizeRestoreMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	opts.Mode = mode
	restoreStarted := time.Now()
	rec, err := r.store.Inspect(vmRef)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for restore: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck

	rec, err = r.store.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	if rec.State != vmstore.StateRunning && rec.State != vmstore.StateStopped && rec.State != vmstore.StateError {
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

	snapshotStore := snapshot.NewStore(r.store.RootDir())
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
	staged, err := stageNativeRestore(ctx, snapshotRec, manifest, rec, opts.Mode)
	if err != nil {
		return nil, err
	}
	defer staged.cleanup() //nolint:errcheck

	observed := r.applyObservation(rec)
	if observed.ObservedState == vmstore.ObservedStateRunning || observed.ObservedState == vmstore.ObservedStatePaused {
		if _, err := r.stopVMLocked(ctx, rec.ID, backend.StopOptions{Force: true, Timeout: 5 * time.Second}); err != nil {
			return nil, fmt.Errorf("stop VM for restore: %w", err)
		}
	}
	dirty, err := r.store.BeginRestore(rec.ID, snapshotRec.ID, opts.Mode)
	if err != nil {
		return nil, fmt.Errorf("mark restore dirty: %w", err)
	}
	fail := func(cause error) (*vmstore.VMRecord, error) {
		_, markErr := r.store.MarkRestoreFailed(rec.ID, cause.Error())
		return nil, errors.Join(cause, markErr)
	}
	if err := staged.commitDisks(); err != nil {
		return fail(fmt.Errorf("replace writable disks: %w", err))
	}
	result, err := restorer.RestoreVM(ctx, dirty, staged.nativeDir, opts.Mode)
	if err != nil {
		return fail(fmt.Errorf("restore backend state: %w", err))
	}
	if err := thawRestoredSnapshot(ctx, dirty, manifest); err != nil {
		cleanupRec := *dirty
		cleanupRec.PID = result.PID
		cleanupRec.APISocket = result.APISocket
		_, _ = r.backend.StopVM(&cleanupRec, backend.StopOptions{Force: true})
		return fail(err)
	}
	restored, err := r.store.MarkRestored(rec.ID, result.PID, result.APISocket, time.Since(restoreStarted))
	if err != nil {
		cleanupRec := *dirty
		cleanupRec.PID = result.PID
		cleanupRec.APISocket = result.APISocket
		_, _ = r.backend.StopVM(&cleanupRec, backend.StopOptions{Force: true})
		return fail(fmt.Errorf("publish restored VM state: %w", err))
	}
	_ = writeVMEvent(restored, "snapshot.restore.completed", vmstore.Observation{
		State: vmstore.ObservedStateRunning, Reason: "native snapshot " + snapshotRec.ID + " restored", CheckedAt: time.Now().UTC(),
	})
	return r.applyObservation(restored), nil
}

func stageNativeRestore(ctx context.Context, snapshotRec *snapshot.Record, manifest *snapshot.Manifest, rec *vmstore.VMRecord, mode string) (*stagedRestore, error) {
	root := filepath.Join(rec.RunDir, ".restore-staging")
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("clear restore staging: %w", err)
	}
	nativeDir := filepath.Join(root, "native")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create native restore staging: %w", err)
	}
	staged := &stagedRestore{nativeDir: nativeDir}
	ok := false
	defer func() {
		if !ok {
			_ = staged.cleanup()
		}
	}()
	for _, file := range manifest.Native.Files {
		if !strings.HasPrefix(file.Path, "native/") || filepath.Base(file.Path) != strings.TrimPrefix(file.Path, "native/") {
			return nil, fmt.Errorf("SNAPSHOT_CORRUPT: invalid native payload path %s", file.Path)
		}
		source := filepath.Join(snapshotRec.DataDir, filepath.FromSlash(file.Path))
		destination := filepath.Join(nativeDir, filepath.Base(file.Path))
		if restoreModePinsSnapshot(mode) && snapshot.IsNativeMemoryFile(file.Path) {
			if err := linkNativeMemory(source, destination); err != nil {
				return nil, fmt.Errorf("link native memory payload %s: %w", file.Path, err)
			}
			continue
		}
		result, err := storage.CopyFile(ctx, source, destination)
		if err != nil {
			return nil, fmt.Errorf("stage native payload %s: %w", file.Path, err)
		}
		if result.SHA256 != file.SHA256 {
			return nil, fmt.Errorf("CHECKSUM_MISMATCH: staged %s", file.Path)
		}
	}
	targets := make(map[string]vmstore.StorageConfig, len(rec.StorageConfigs))
	for _, disk := range rec.StorageConfigs {
		if disk.EffectiveRole() == vmstore.StorageRoleCOW || disk.EffectiveRole() == vmstore.StorageRoleData {
			targets[disk.ID] = disk
		}
	}
	for _, disk := range manifest.Disks {
		target, found := targets[disk.ID]
		if !found {
			return nil, fmt.Errorf("SNAPSHOT_INCOMPATIBLE: no writable target for disk %s", disk.ID)
		}
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
			return nil, fmt.Errorf("create target directory for disk %s: %w", disk.ID, err)
		}
		stagedPath := filepath.Join(filepath.Dir(target.Path), ".kumabox-restore-"+snapshotRec.ID+"-"+filepath.Base(target.Path))
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("clear staged disk %s: %w", disk.ID, err)
		}
		source := filepath.Join(snapshotRec.DataDir, filepath.FromSlash(disk.Path))
		result, err := storage.CopyFile(ctx, source, stagedPath)
		if err != nil {
			return nil, fmt.Errorf("stage writable disk %s: %w", disk.ID, err)
		}
		if result.SHA256 != disk.SHA256 {
			return nil, fmt.Errorf("CHECKSUM_MISMATCH: staged disk %s", disk.ID)
		}
		staged.disks = append(staged.disks, stagedRestoreDisk{id: disk.ID, target: target.Path, staged: stagedPath})
	}
	if len(staged.disks) != len(targets) {
		return nil, errors.New("SNAPSHOT_INCOMPATIBLE: writable disk set is incomplete")
	}
	ok = true
	return staged, nil
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
	errs := []error{os.RemoveAll(filepath.Dir(s.nativeDir))}
	for _, disk := range s.disks {
		if err := os.Remove(disk.staged); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck
	return dir.Sync()
}
