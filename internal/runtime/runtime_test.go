package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type backendFake struct {
	render  func(*vmstore.VMRecord) error
	start   func(*vmstore.VMRecord) (*backend.StartResult, error)
	stop    func(*vmstore.VMRecord, backend.StopOptions) (*backend.StopResult, error)
	observe func(*vmstore.VMRecord) vmstore.Observation
}

func (b backendFake) RenderConfig(rec *vmstore.VMRecord) error {
	return b.render(rec)
}

func (b backendFake) StartVM(rec *vmstore.VMRecord) (*backend.StartResult, error) {
	return b.start(rec)
}

func (b backendFake) StopVM(rec *vmstore.VMRecord, opts backend.StopOptions) (*backend.StopResult, error) {
	if b.stop != nil {
		return b.stop(rec, opts)
	}
	return &backend.StopResult{}, nil
}

func (b backendFake) ObserveVM(rec *vmstore.VMRecord) vmstore.Observation {
	if b.observe != nil {
		return b.observe(rec)
	}
	return vmstore.Observation{
		State:     vmstore.ObservedStateCreated,
		Reason:    "test observation",
		CheckedAt: time.Now().UTC(),
	}
}

func TestCreateVMRollsBackRecordOnRenderFailure(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	renderErr := errors.New("render failed")
	rt := NewWithBackend(store, backendFake{
		render: func(*vmstore.VMRecord) error { return renderErr },
	})

	_, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "rollback",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if !errors.Is(err, renderErr) {
		t.Fatalf("error = %v, want %v", err, renderErr)
	}

	if _, err := store.Inspect("rollback"); !errors.Is(err, vmstore.ErrNotFound) {
		t.Fatalf("inspect after rollback error = %v", err)
	}
}

func TestStartVMMarksRunning(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 1234, APISocket: "/tmp/ch.sock"}, nil
			},
			observe: func(*vmstore.VMRecord) vmstore.Observation {
				return vmstore.Observation{
					State:     vmstore.ObservedStateRunning,
					Reason:    "running",
					CheckedAt: time.Now().UTC(),
				}
			},
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "start-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	started, err := rt.StartVM(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != vmstore.StateRunning {
		t.Fatalf("state = %s", started.State)
	}
	if started.PID != 1234 || started.APISocket != "/tmp/ch.sock" {
		t.Fatalf("runtime fields = pid %d socket %s", started.PID, started.APISocket)
	}
	if started.ObservedState != vmstore.ObservedStateRunning {
		t.Fatalf("observed state = %s", started.ObservedState)
	}
}

func TestStartVMRerendersAfterFirstBoot(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	var renderFirstBooted []bool
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(rec *vmstore.VMRecord) error {
				renderFirstBooted = append(renderFirstBooted, rec.FirstBooted)
				return nil
			},
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 1234, APISocket: filepath.Join(dir, "run", "ch.sock")}, nil
			},
			observe: func(rec *vmstore.VMRecord) vmstore.Observation {
				state := vmstore.ObservedStateCreated
				if rec.State == vmstore.StateRunning {
					state = vmstore.ObservedStateRunning
				}
				if rec.State == vmstore.StateStopped {
					state = vmstore.ObservedStateStopped
				}
				return vmstore.Observation{
					State:     state,
					Reason:    string(state),
					CheckedAt: time.Now().UTC(),
				}
			},
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "cloudimg",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StopVM(rec.ID, backend.StopOptions{Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}

	if len(renderFirstBooted) != 3 {
		t.Fatalf("render calls = %v", renderFirstBooted)
	}
	if renderFirstBooted[0] || renderFirstBooted[1] {
		t.Fatalf("first boot renders should include cidata: %v", renderFirstBooted)
	}
	if !renderFirstBooted[2] {
		t.Fatalf("second start should render with firstBooted=true: %v", renderFirstBooted)
	}
}

func TestStartVMMarksErrorOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	startErr := errors.New("start failed")
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start:  func(*vmstore.VMRecord) (*backend.StartResult, error) { return nil, startErr },
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "fail-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.StartVM(rec.ID); !errors.Is(err, startErr) {
		t.Fatalf("start error = %v", err)
	}
	updated, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != vmstore.StateError || updated.Error == "" {
		t.Fatalf("updated record = %+v", updated)
	}
}

func TestInspectVMReconcilesStaleRunningRecord(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	checkedAt := time.Date(2026, 6, 29, 1, 2, 3, 0, time.UTC)
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 4321, APISocket: filepath.Join(dir, "run", "ch.sock")}, nil
			},
			observe: func(rec *vmstore.VMRecord) vmstore.Observation {
				if rec.State == vmstore.StateRunning {
					return vmstore.Observation{
						State:     vmstore.ObservedStateStopped,
						Reason:    "process 4321 is not alive",
						CheckedAt: checkedAt,
					}
				}
				return vmstore.Observation{
					State:     vmstore.ObservedStateCreated,
					Reason:    "created",
					CheckedAt: checkedAt,
				}
			},
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "stale",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}

	inspected, err := rt.InspectVM(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.State != vmstore.StateRunning {
		t.Fatalf("persisted state = %s", inspected.State)
	}
	if inspected.ObservedState != vmstore.ObservedStateStopped {
		t.Fatalf("observed state = %s", inspected.ObservedState)
	}
	if inspected.ObservedReason == "" || inspected.ObservedAt == nil {
		t.Fatalf("missing observation detail: %+v", inspected)
	}

	raw, err := os.ReadFile(filepath.Join(inspected.LogDir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "backend.exit.detected") {
		t.Fatalf("events log missing backend.exit.detected: %s", raw)
	}
}

func TestStopVMMarksStopped(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	stopCalled := false
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 12345, APISocket: filepath.Join(dir, "run", "ch.sock")}, nil
			},
			stop: func(rec *vmstore.VMRecord, opts backend.StopOptions) (*backend.StopResult, error) {
				stopCalled = true
				if rec.PID != 12345 {
					t.Fatalf("stop pid = %d", rec.PID)
				}
				if opts.Timeout <= 0 {
					t.Fatal("expected timeout")
				}
				return &backend.StopResult{}, nil
			},
			observe: func(rec *vmstore.VMRecord) vmstore.Observation {
				state := vmstore.ObservedStateCreated
				reason := "created"
				if rec.State == vmstore.StateRunning {
					state = vmstore.ObservedStateRunning
					reason = "running"
				}
				if rec.State == vmstore.StateStopped {
					state = vmstore.ObservedStateStopped
					reason = "stopped"
				}
				return vmstore.Observation{
					State:     state,
					Reason:    reason,
					CheckedAt: time.Now().UTC(),
				}
			},
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "stop-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}

	stopped, err := rt.StopVM(rec.ID, backend.StopOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !stopCalled {
		t.Fatal("backend stop was not called")
	}
	if stopped.State != vmstore.StateStopped {
		t.Fatalf("state = %s", stopped.State)
	}
	if stopped.PID != 0 || stopped.APISocket != "" {
		t.Fatalf("runtime fields not cleared: %+v", stopped)
	}
	if stopped.ObservedState != vmstore.ObservedStateStopped {
		t.Fatalf("observed state = %s", stopped.ObservedState)
	}

	raw, err := os.ReadFile(filepath.Join(stopped.LogDir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "backend.stop.completed") {
		t.Fatalf("events log missing backend.stop.completed: %s", raw)
	}
}

func TestLogsVMTailsKnownLogFiles(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "logs",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.LogDir, "cloud-hypervisor.stdout.log"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log"), []byte("err-one\nerr-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.LogDir, "console.log"), []byte("console-one\nconsole-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logs, err := rt.LogsVM("logs", LogOptions{Tail: 1})
	if err != nil {
		t.Fatal(err)
	}
	if logs.VMID != rec.ID || logs.Name != rec.Name {
		t.Fatalf("logs identity = %+v", logs)
	}
	if len(logs.Files) != 1 {
		t.Fatalf("log file count = %d", len(logs.Files))
	}
	if logs.Files[0].Name != "console.log" || logs.Files[0].Content != "console-two\n" {
		t.Fatalf("console tail = %+v", logs.Files[0])
	}

	vmmLogs, err := rt.LogsVM("logs", LogOptions{Tail: 2, Source: LogSourceVMM})
	if err != nil {
		t.Fatal(err)
	}
	if len(vmmLogs.Files) != 2 {
		t.Fatalf("vmm log file count = %d", len(vmmLogs.Files))
	}
	if vmmLogs.Files[0].Name != "cloud-hypervisor.stdout.log" || vmmLogs.Files[0].Content != "two\nthree\n" {
		t.Fatalf("stdout tail = %+v", vmmLogs.Files[0])
	}
	if vmmLogs.Files[1].Name != "cloud-hypervisor.stderr.log" || vmmLogs.Files[1].Content != "err-one\nerr-two\n" {
		t.Fatalf("stderr tail = %+v", vmmLogs.Files[1])
	}
}

func TestDeleteVMRemovesRecordAndManagedDirsOnly(t *testing.T) {
	dir := t.TempDir()
	rootDisk := filepath.Join(dir, "fixtures", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(rootDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDisk, []byte("root disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "delete-me",
		RootDisk: rootDisk,
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec.RunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	deleted, err := rt.DeleteVM("delete-me", false)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != rec.ID {
		t.Fatalf("deleted ID = %s, want %s", deleted.ID, rec.ID)
	}
	if _, err := store.Inspect(rec.ID); !errors.Is(err, vmstore.ErrNotFound) {
		t.Fatalf("inspect after delete error = %v", err)
	}
	if _, err := os.Stat(rootDisk); err != nil {
		t.Fatalf("root disk should remain: %v", err)
	}
	if _, err := os.Stat(rec.RunDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir still exists or unexpected error: %v", err)
	}
	if _, err := os.Stat(rec.LogDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log dir still exists or unexpected error: %v", err)
	}
}

func TestDeleteVMRequiresForceForRunningVM(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	stopCalled := false
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 12345, APISocket: filepath.Join(dir, "run", "ch.sock")}, nil
			},
			stop: func(*vmstore.VMRecord, backend.StopOptions) (*backend.StopResult, error) {
				stopCalled = true
				return &backend.StopResult{}, nil
			},
			observe: func(rec *vmstore.VMRecord) vmstore.Observation {
				state := vmstore.ObservedStateCreated
				if rec.State == vmstore.StateRunning {
					state = vmstore.ObservedStateRunning
				}
				if rec.State == vmstore.StateStopped {
					state = vmstore.ObservedStateStopped
				}
				return vmstore.Observation{
					State:     state,
					Reason:    string(state),
					CheckedAt: time.Now().UTC(),
				}
			},
		},
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "running-delete",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := rt.DeleteVM(rec.ID, false); err == nil {
		t.Fatal("expected delete running VM without force to fail")
	}
	if stopCalled {
		t.Fatal("stop should not be called without force")
	}
	if _, err := store.Inspect(rec.ID); err != nil {
		t.Fatalf("record should remain after failed delete: %v", err)
	}

	if _, err := rt.DeleteVM(rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if !stopCalled {
		t.Fatal("force delete did not stop VM")
	}
	if _, err := store.Inspect(rec.ID); !errors.Is(err, vmstore.ErrNotFound) {
		t.Fatalf("inspect after force delete error = %v", err)
	}
}

func TestStopVMPreservesNetworkResources(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	store := vmstore.New(rootDir)
	rt := NewWithBackend(
		store,
		backendFake{
			render: func(*vmstore.VMRecord) error { return nil },
			start: func(*vmstore.VMRecord) (*backend.StartResult, error) {
				return &backend.StartResult{PID: 12345, APISocket: filepath.Join(dir, "run", "ch.sock")}, nil
			},
			observe: func(rec *vmstore.VMRecord) vmstore.Observation {
				state := vmstore.ObservedStateCreated
				if rec.State == vmstore.StateRunning {
					state = vmstore.ObservedStateRunning
				}
				if rec.State == vmstore.StateStopped {
					state = vmstore.ObservedStateStopped
				}
				return vmstore.Observation{
					State:     state,
					Reason:    string(state),
					CheckedAt: time.Now().UTC(),
				}
			},
		},
	)
	rt.cfg = testRuntimeConfig(rootDir)

	rec, allocation := createVMWithNetwork(t, rt, store, "stop-network")
	if _, err := rt.StartVM(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StopVM(rec.ID, backend.StopOptions{Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}

	networkStore := kbnetwork.NewStore(rootDir)
	records, err := networkStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != allocation.Record.ID {
		t.Fatalf("network records after stop = %+v", records)
	}
	leases, err := networkStore.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leases[allocation.Config.Network.IP]; !ok {
		t.Fatalf("lease was removed on stop: %+v", leases)
	}
}

func TestDeleteVMCleansNetworkResources(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	store := vmstore.New(rootDir)
	rt := NewWithBackend(store, backendFake{render: func(*vmstore.VMRecord) error { return nil }})
	rt.cfg = testRuntimeConfig(rootDir)
	deletedTaps := []string{}
	withDeleteHostTap(t, func(tap string) error {
		deletedTaps = append(deletedTaps, tap)
		return nil
	})

	rec, allocation := createVMWithNetwork(t, rt, store, "delete-network")
	if _, err := rt.DeleteVM(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(deletedTaps) != 1 || deletedTaps[0] != allocation.Record.TAP {
		t.Fatalf("deleted taps = %+v", deletedTaps)
	}
	if _, err := store.Inspect(rec.ID); !errors.Is(err, vmstore.ErrNotFound) {
		t.Fatalf("inspect after delete error = %v", err)
	}
	networkStore := kbnetwork.NewStore(rootDir)
	records, err := networkStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("network records after delete = %+v", records)
	}
	leases, err := networkStore.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases after delete = %+v", leases)
	}
}

func TestDeleteVMMarksNetworkCleanupPendingOnFailure(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	store := vmstore.New(rootDir)
	rt := NewWithBackend(store, backendFake{render: func(*vmstore.VMRecord) error { return nil }})
	rt.cfg = testRuntimeConfig(rootDir)
	tapErr := errors.New("tap delete failed")
	withDeleteHostTap(t, func(string) error { return tapErr })

	rec, allocation := createVMWithNetwork(t, rt, store, "pending-network")
	if _, err := rt.DeleteVM(rec.ID, false); !errors.Is(err, tapErr) {
		t.Fatalf("delete error = %v, want %v", err, tapErr)
	}
	if _, err := store.Inspect(rec.ID); err != nil {
		t.Fatalf("VM record should remain after cleanup failure: %v", err)
	}
	networkStore := kbnetwork.NewStore(rootDir)
	records, err := networkStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != allocation.Record.ID {
		t.Fatalf("network records after failure = %+v", records)
	}
	if !records[0].Cleanup.Pending || !strings.Contains(records[0].Cleanup.Reason, "tap delete failed") {
		t.Fatalf("cleanup = %+v", records[0].Cleanup)
	}
	leases, err := networkStore.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leases[allocation.Config.Network.IP]; !ok {
		t.Fatalf("lease should remain after tap delete failure: %+v", leases)
	}
}

func testRuntimeConfig(rootDir string) config.Config {
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	return cfg
}

func withDeleteHostTap(t *testing.T, fn func(string) error) {
	t.Helper()
	previous := deleteHostTap
	deleteHostTap = fn
	t.Cleanup(func() {
		deleteHostTap = previous
	})
}

func createVMWithNetwork(
	t *testing.T,
	rt *Runtime,
	store *vmstore.Store,
	name string,
) (*vmstore.VMRecord, *kbnetwork.Allocation) {
	t.Helper()
	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     name,
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(t.TempDir(), "run"),
		LogDir:   filepath.Join(t.TempDir(), "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := kbnetwork.NewAllocator(rt.cfg.Runtime.RootDir, rt.cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    rec.ID,
		Network: "default",
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := kbnetwork.NewStore(rt.cfg.Runtime.RootDir).UpsertRecord(allocation.Record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetNetworkConfigs(rec.ID, []kbnetwork.Config{allocation.Config}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated, allocation
}
