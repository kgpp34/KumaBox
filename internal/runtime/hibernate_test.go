package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestHibernateVMPersistsBeforeStopping(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vmstore.ObservedStateRunning
	steps := make([]string, 0, 3)
	rt.backend = backendFake{
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			steps = append(steps, "pause")
			backendState = vmstore.ObservedStatePaused
			return nil
		},
		snapshot: func(_ context.Context, _ *vmstore.VMRecord, destination string) error {
			steps = append(steps, "snapshot")
			return writeNativeSnapshotFixture(destination, rec)
		},
		stop: func(*vmstore.VMRecord, backend.StopOptions) (*backend.StopResult, error) {
			if records, err := snapshot.NewStore(store.RootDir()).List(); err != nil || len(records) != 1 {
				t.Fatalf("snapshot was not durable before stop: %+v, %v", records, err)
			}
			steps = append(steps, "stop")
			backendState = vmstore.ObservedStateStopped
			return &backend.StopResult{}, nil
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			t.Fatal("successful hibernate resumed the VM")
			return nil
		},
	}

	result, err := rt.HibernateVM(context.Background(), rec.ID, HibernateOptions{Name: "nap"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "pause,snapshot,stop" {
		t.Fatalf("steps = %v", steps)
	}
	if result.VM.State != vmstore.StateStopped || result.VM.Hibernate == nil || result.VM.Hibernate.SnapshotID != result.Snapshot.ID {
		t.Fatalf("hibernate result = %+v", result)
	}
	if _, err := rt.StartVMContext(context.Background(), rec.ID); err == nil || !strings.Contains(err.Error(), "VM_HIBERNATED") {
		t.Fatalf("cold start error = %v", err)
	}
}

func TestHibernateVMResumesWhenPersistenceFails(t *testing.T) {
	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vmstore.ObservedStateRunning
	resumed := false
	rt.backend = backendFake{
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			backendState = vmstore.ObservedStatePaused
			return nil
		},
		snapshot: func(context.Context, *vmstore.VMRecord, string) error {
			return errors.New("injected persistence failure")
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			resumed = true
			backendState = vmstore.ObservedStateRunning
			return nil
		},
	}
	if _, err := rt.HibernateVM(context.Background(), rec.ID, HibernateOptions{Name: "failed-nap"}); err == nil {
		t.Fatal("expected hibernate failure")
	}
	if !resumed {
		t.Fatal("VM was not resumed after persistence failure")
	}
	if records, err := snapshot.NewStore(store.RootDir()).Scan(); err != nil || len(records) != 0 {
		t.Fatalf("failed hibernate leaked snapshots: %+v, %v", records, err)
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != vmstore.StateRunning || persisted.Hibernate != nil {
		t.Fatalf("source state = %+v", persisted)
	}
}

func TestMarkRestoredClearsHibernateState(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rec, err := store.Create(vmstore.CreateRequest{
		Name: "wake", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteHibernate(rec.ID, "snap_nap"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRestore(rec.ID, "snap_nap", "copy"); err != nil {
		t.Fatal(err)
	}
	woken, err := store.CompleteRestore(rec.ID, 42, filepath.Join(rec.RunDir, "ch.sock"), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if woken.Hibernate != nil || woken.State != vmstore.StateRunning {
		t.Fatalf("woken record = %+v", woken)
	}
}
