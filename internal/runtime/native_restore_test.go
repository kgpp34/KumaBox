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
	"github.com/kumabox/kumabox/internal/vmstore"
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
	backendState := vmstore.ObservedStateRunning
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
	if restored.ID != rec.ID || restored.Name != rec.Name || restored.State != vmstore.StateRunning || restored.Restore != nil {
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
	backendState := vmstore.ObservedStateRunning
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
	if persisted.State != vmstore.StateError || persisted.Restore == nil || persisted.Restore.State != "failed" {
		t.Fatalf("failed restore record = %+v", persisted)
	}
	if _, err := rt.StartVM(rec.ID); err == nil || !containsError(err, "VM_RESTORE_DIRTY") {
		t.Fatalf("start error = %v", err)
	}
}

func TestRestoreNativeVMSucceedsWithoutGuestAgent(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vmstore.ObservedStateRunning
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
	if restored.State != vmstore.StateRunning || persisted.State != vmstore.StateRunning || persisted.Restore != nil {
		t.Fatalf("restored record = %+v persisted = %+v", restored, persisted)
	}
	if restored.LastRestore == nil || !strings.Contains(restored.LastRestore.GuestAgentWarning, agentErr.Error()) {
		t.Fatalf("restore warning = %+v", restored.LastRestore)
	}
}

func nativeRestoreBackend(t *testing.T, rec *vmstore.VMRecord, state *vmstore.ObservedState, restoreErr error) backendFake {
	t.Helper()
	originalReseed := reseedRestoredGuest
	reseedRestoredGuest = func(context.Context, string, bool) error { return nil }
	t.Cleanup(func() { reseedRestoredGuest = originalReseed })
	return backendFake{
		render: func(*vmstore.VMRecord) error { return nil },
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: *state, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			*state = vmstore.ObservedStatePaused
			return nil
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			*state = vmstore.ObservedStateRunning
			return nil
		},
		snapshot: func(_ context.Context, _ *vmstore.VMRecord, destination string) error {
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
		stop: func(*vmstore.VMRecord, backend.StopOptions) (*backend.StopResult, error) {
			*state = vmstore.ObservedStateStopped
			return &backend.StopResult{}, nil
		},
		restore: func(_ context.Context, dirty *vmstore.VMRecord, sourceDir, mode string) (*backend.StartResult, error) {
			if dirty.Restore == nil || dirty.Restore.State != "dirty" || mode != "copy" {
				t.Fatalf("restore input = %+v mode=%s", dirty.Restore, mode)
			}
			if _, err := os.Stat(filepath.Join(sourceDir, "memory-range-0")); err != nil {
				t.Fatal(err)
			}
			if restoreErr != nil {
				return nil, restoreErr
			}
			*state = vmstore.ObservedStateRunning
			return &backend.StartResult{PID: 4321, APISocket: filepath.Join(rec.RunDir, "ch.sock")}, nil
		},
	}
}

func containsError(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}
