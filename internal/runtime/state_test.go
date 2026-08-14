package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vm"
)

func TestPauseResumePersistsLiveState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vm.New(filepath.Join(dir, "data"))
	backendState := vm.ObservedStateRunning
	pauseCalls := 0
	resumeCalls := 0
	rt := NewWithBackend(store, backendFake{
		render: func(*vm.VMRecord) error { return nil },
		start: func(*vm.VMRecord) (*backend.StartResult, error) {
			return &backend.StartResult{PID: 1234, APISocket: "/tmp/ch.sock"}, nil
		},
		pause: func(context.Context, *vm.VMRecord) error {
			pauseCalls++
			backendState = vm.ObservedStatePaused
			return nil
		},
		resume: func(context.Context, *vm.VMRecord) error {
			resumeCalls++
			backendState = vm.ObservedStateRunning
			return nil
		},
		observe: func(*vm.VMRecord) vm.Observation {
			return vm.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
	})
	rec, err := rt.CreateVM(vm.CreateRequest{
		Name: "state", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"), Network: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkStarted(rec.ID, 1234, "/tmp/ch.sock"); err != nil {
		t.Fatal(err)
	}

	paused, err := rt.PauseVM(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != vm.StatePaused || paused.ObservedState != vm.ObservedStatePaused || paused.PID != 1234 {
		t.Fatalf("paused record = %+v", paused)
	}
	if _, err := rt.PauseVM(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := rt.ResumeVM(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != vm.StateRunning || resumed.ObservedState != vm.ObservedStateRunning || resumed.PID != 1234 {
		t.Fatalf("resumed record = %+v", resumed)
	}
	if _, err := rt.ResumeVM(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if pauseCalls != 1 || resumeCalls != 1 {
		t.Fatalf("transition calls pause=%d resume=%d", pauseCalls, resumeCalls)
	}
}
