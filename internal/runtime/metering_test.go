package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/metering"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestLifecycleRecordsComputeUsageIntervals(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	backendState := vmstore.ObservedStateCreated
	rt := NewWithBackend(store, backendFake{
		render: func(*vmstore.VMRecord) error { return nil },
		start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
			backendState = vmstore.ObservedStateRunning
			return &backend.StartResult{PID: 1234, APISocket: "ch.sock"}, nil
		},
		stop: func(*vmstore.VMRecord, backend.StopOptions) (*backend.StopResult, error) {
			backendState = vmstore.ObservedStateStopped
			return &backend.StopResult{}, nil
		},
		pause: func(context.Context, *vmstore.VMRecord) error { backendState = vmstore.ObservedStatePaused; return nil },
		resume: func(context.Context, *vmstore.VMRecord) error {
			backendState = vmstore.ObservedStateRunning
			return nil
		},
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
	})
	rec, err := rt.CreateVM(vmstore.CreateRequest{Name: "metered", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"), Network: "none", CPUs: 2, MemoryBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVMContext(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.PauseVM(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ResumeVM(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StopVMContext(t.Context(), rec.ID, backend.StopOptions{}); err != nil {
		t.Fatal(err)
	}
	usage, err := rt.storeSet.Metering.Usage(t.Context(), metering.Query{VMRef: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage[0].StartReason != metering.ReasonBoot || usage[0].EndReason != metering.ReasonPause || usage[1].StartReason != metering.ReasonResume || usage[1].EndReason != metering.ReasonStopUser {
		t.Fatalf("usage = %+v", usage)
	}
	for _, interval := range usage {
		if interval.VCPUs != 2 || interval.MemoryBytes != 1024 || interval.EndedAt == nil {
			t.Fatalf("interval = %+v", interval)
		}
	}
}

func TestReconcileMeteringIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(store, backendFake{})
	rec, err := store.Create(vmstore.CreateRequest{Name: "reconcile", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkStarted(rec.ID, 1234, "ch.sock"); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReconcileMetering(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReconcileMetering(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := rt.storeSet.Metering.Events(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != metering.KindComputeStart {
		t.Fatalf("events = %+v", events)
	}
}
