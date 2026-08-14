package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vm"
)

func TestLinkNativeMemorySharesSourceInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "memory-range-0")
	destination := filepath.Join(dir, "linked-memory-range-0")
	if err := os.WriteFile(source, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkNativeMemory(source, destination); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, destinationInfo) {
		t.Fatal("linked memory does not share the source inode")
	}
}

func TestRestoreNativeVMReplacesWritableStateAndResumesIdentity(t *testing.T) {
	rt, store, rec, sourceDisk := newRunningSnapshotRuntime(t)
	backendState := vm.ObservedStateRunning
	rt.backend = nativeRestoreBackend(t, rec, &backendState, nil)
	originalReseed := reseedRestoredGuest
	t.Cleanup(func() { reseedRestoredGuest = originalReseed })
	var reseedCalled bool
	reseedRestoredGuest = func(_ context.Context, socket string, regenerateMachineID bool) error {
		reseedCalled = true
		if socket != rec.VsockSocket || regenerateMachineID {
			t.Fatalf("restore reseed socket=%q regenerateMachineID=%t", socket, regenerateMachineID)
		}
		return nil
	}

	ready, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "restore-source")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceDisk, []byte("after-snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := rt.RestoreNativeVM(context.Background(), rec.ID, ready.ID, NativeRestoreOptions{Mode: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != rec.ID || restored.Name != rec.Name || restored.State != vm.StateRunning || restored.Restore != nil {
		t.Fatalf("restored record = %+v", restored)
	}
	content, err := os.ReadFile(sourceDisk)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "writable" {
		t.Fatalf("restored disk = %q", content)
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Restore != nil || persisted.PID != 4321 {
		t.Fatalf("persisted record = %+v", persisted)
	}
	if persisted.LastRestore == nil || persisted.LastRestore.BackendRestoreDurationMs < 0 || persisted.LastRestore.ReadinessDurationMs < 0 {
		t.Fatalf("restore metrics = %+v", persisted.LastRestore)
	}
	if !reseedCalled || persisted.LastRestore.GuestAgentWarning != "" {
		t.Fatalf("restore reseed called=%t warning=%q", reseedCalled, persisted.LastRestore.GuestAgentWarning)
	}
}

func TestRestoreNativeVMFailureQuarantinesColdStart(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vm.ObservedStateRunning
	restoreErr := errors.New("injected backend restore failure")
	rt.backend = nativeRestoreBackend(t, rec, &backendState, restoreErr)
	ready, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "restore-failure")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.RestoreNativeVM(context.Background(), rec.ID, ready.ID, NativeRestoreOptions{}); !errors.Is(err, restoreErr) {
		t.Fatalf("restore error = %v", err)
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != vm.StateError || persisted.Restore == nil || persisted.Restore.State != "failed" {
		t.Fatalf("failed restore record = %+v", persisted)
	}
	if _, err := rt.StartVM(rec.ID); err == nil || !containsError(err, "VM_RESTORE_DIRTY") {
		t.Fatalf("start error = %v", err)
	}
}

func TestRestoreNativeVMSucceedsWithoutGuestAgent(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vm.ObservedStateRunning
	rt.backend = nativeRestoreBackend(t, rec, &backendState, nil)
	agentErr := errors.New("agent unavailable")
	reseedRestoredGuest = func(context.Context, string, bool) error { return agentErr }
	ready, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "restore-readiness-failure")
	if err != nil {
		t.Fatal(err)
	}

	restored, err := rt.RestoreNativeVM(context.Background(), rec.ID, ready.ID, NativeRestoreOptions{})
	if err != nil {
		t.Fatalf("restore error = %v", err)
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != vm.StateRunning || persisted.State != vm.StateRunning || persisted.Restore != nil {
		t.Fatalf("restored record = %+v persisted = %+v", restored, persisted)
	}
	if restored.LastRestore == nil || !strings.Contains(restored.LastRestore.GuestAgentWarning, agentErr.Error()) {
		t.Fatalf("restore warning = %+v", restored.LastRestore)
	}
}

func TestRestoreNativeVMStopsBackendWhenSnapshotReferenceFails(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vm.ObservedStateRunning
	referenceErr := errors.New("injected reference failure")
	backendImpl := nativeRestoreBackend(t, rec, &backendState, nil)
	baseStop := backendImpl.stop
	var restoredBackendStopped bool
	backendImpl.stop = func(stopped *vm.VMRecord, options backend.StopOptions) (*backend.StopResult, error) {
		if stopped.PID == 4321 {
			restoredBackendStopped = true
		}
		return baseStop(stopped, options)
	}
	rt.backend = backendImpl
	ready, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "restore-reference-failure")
	if err != nil {
		t.Fatal(err)
	}
	rt.storeSet.References = failingReferenceState{ReferenceState: rt.storeSet.References, err: referenceErr}

	_, err = rt.RestoreNativeVM(context.Background(), rec.ID, ready.ID, NativeRestoreOptions{})
	if !errors.Is(err, referenceErr) {
		t.Fatalf("restore error = %v, want %v", err, referenceErr)
	}
	if !restoredBackendStopped {
		t.Fatal("restored backend was not stopped after reference failure")
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != vm.StateError || persisted.PID != 0 {
		t.Fatalf("failed restore record = %+v", persisted)
	}
	if _, err := os.Stat(filepath.Join(rec.RunDir, ".restore-staging")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore staging remains after backend stop: %v", err)
	}
}

func TestRestoreNativeVMPreservesOnDemandMemoryWhenRollbackStopFails(t *testing.T) {
	rt, _, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vm.ObservedStateRunning
	referenceErr := errors.New("injected reference failure")
	stopErr := errors.New("injected stop failure")
	backendImpl := nativeRestoreBackend(t, rec, &backendState, nil)
	backendImpl.nativeHost = func(context.Context, *vm.VMRecord) (backend.NativeHost, error) {
		return backend.NativeHost{RestoreModes: []string{string(RestoreModeOnDemand)}}, nil
	}
	backendImpl.stop = func(stopped *vm.VMRecord, _ backend.StopOptions) (*backend.StopResult, error) {
		if stopped.PID == 4321 {
			return nil, stopErr
		}
		backendState = vm.ObservedStateStopped
		return &backend.StopResult{}, nil
	}
	rt.backend = backendImpl
	ready, err := rt.CreateRunningSnapshot(t.Context(), rec.ID, "restore-stop-failure")
	if err != nil {
		t.Fatal(err)
	}
	rt.storeSet.References = failingReferenceState{ReferenceState: rt.storeSet.References, err: referenceErr}

	_, err = rt.RestoreNativeVM(t.Context(), rec.ID, ready.ID, NativeRestoreOptions{Mode: RestoreModeOnDemand})
	if !errors.Is(err, referenceErr) || !errors.Is(err, stopErr) {
		t.Fatalf("restore error = %v, want reference and stop failures", err)
	}
	if _, err := os.Stat(filepath.Join(rec.RunDir, ".restore-staging", "native", "memory-range-0")); err != nil {
		t.Fatalf("on-demand memory payload was removed while backend may be running: %v", err)
	}
}

type failingReferenceState struct {
	state.ReferenceState
	err error
}

func (s failingReferenceState) Upsert(ctx context.Context, record reference.Record) error {
	if record.TargetKind == referenceKindSnapshot {
		return s.err
	}
	return s.ReferenceState.Upsert(ctx, record)
}

func nativeRestoreBackend(t *testing.T, rec *vm.VMRecord, state *vm.ObservedState, restoreErr error) backendFake {
	t.Helper()
	originalReseed := reseedRestoredGuest
	reseedRestoredGuest = func(context.Context, string, bool) error { return nil }
	t.Cleanup(func() { reseedRestoredGuest = originalReseed })
	return backendFake{
		render: func(*vm.VMRecord) error { return nil },
		observe: func(*vm.VMRecord) vm.Observation {
			return vm.Observation{State: *state, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vm.VMRecord) error {
			*state = vm.ObservedStatePaused
			return nil
		},
		resume: func(context.Context, *vm.VMRecord) error {
			*state = vm.ObservedStateRunning
			return nil
		},
		snapshot: func(_ context.Context, _ *vm.VMRecord, destination string) error {
			files := map[string]string{
				"config.json": fmt.Sprintf(`{"cpus":{"boot_vcpus":1},"memory":{"size":536870912},"disks":[{"path":%q,"readonly":false}],"vsock":{}}`, rec.StorageConfigs[0].Path),
				"state.json":  "{}", "memory-range-0": "memory",
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0o600); err != nil {
					return err
				}
			}
			return nil
		},
		stop: func(*vm.VMRecord, backend.StopOptions) (*backend.StopResult, error) {
			*state = vm.ObservedStateStopped
			return &backend.StopResult{}, nil
		},
		restore: func(_ context.Context, dirty *vm.VMRecord, sourceDir, mode string) (*backend.StartResult, error) {
			if dirty.Restore == nil || dirty.Restore.State != "dirty" || (mode != "copy" && mode != "ondemand") {
				t.Fatalf("restore input = %+v mode=%s", dirty.Restore, mode)
			}
			if _, err := os.Stat(filepath.Join(sourceDir, "memory-range-0")); err != nil {
				t.Fatal(err)
			}
			if restoreErr != nil {
				return nil, restoreErr
			}
			*state = vm.ObservedStateRunning
			return &backend.StartResult{PID: 4321, APISocket: filepath.Join(rec.RunDir, "ch.sock")}, nil
		},
	}
}

func containsError(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}
