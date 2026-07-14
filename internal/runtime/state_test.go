package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestPauseResumePersistsLiveState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	backendState := vmstore.ObservedStateRunning
	pauseCalls := 0
	resumeCalls := 0
	rt := NewWithBackend(store, backendFake{
		render: func(*vmstore.VMRecord) error { return nil },
		start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
			return &backend.StartResult{PID: 1234, APISocket: "/tmp/ch.sock"}, nil
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			pauseCalls++
			backendState = vmstore.ObservedStatePaused
			return nil
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			resumeCalls++
			backendState = vmstore.ObservedStateRunning
			return nil
		},
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
	})
	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name: "state", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"), Network: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkRunning(rec.ID, 1234, "/tmp/ch.sock"); err != nil {
		t.Fatal(err)
	}

	paused, err := rt.PauseVM(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != vmstore.StatePaused || paused.ObservedState != vmstore.ObservedStatePaused || paused.PID != 1234 {
		t.Fatalf("paused record = %+v", paused)
	}
	if _, err := rt.PauseVM(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := rt.ResumeVM(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != vmstore.StateRunning || resumed.ObservedState != vmstore.ObservedStateRunning || resumed.PID != 1234 {
		t.Fatalf("resumed record = %+v", resumed)
	}
	if _, err := rt.ResumeVM(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if pauseCalls != 1 || resumeCalls != 1 {
		t.Fatalf("transition calls pause=%d resume=%d", pauseCalls, resumeCalls)
	}
}
