package core

import (
	"context"
	"strings"
	"testing"

	filelock "github.com/kumabox/kumabox/internal/lock/flock"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

func TestReconcileSandboxesStopsOnlyAbsentRuntime(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation vmm.ProcessState
		wantAction  string
		wantState   types.SandboxState
	}{
		{name: "absent", observation: vmm.ProcessAbsent, wantAction: "recovered-stopped", wantState: types.SandboxStateStopped},
		{name: "live", observation: vmm.ProcessRunning, wantState: types.SandboxStateRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, steps := newTestSandboxService(t, nil)
			catalog := service.dependencies.catalog.(*fakeCatalog)
			catalog.record = types.Sandbox{
				ID: fixedID, VMM: types.VMMCloudHypervisor,
				Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4,
			}
			testRuntime(t, service).observation = vmm.Observation{State: test.observation}
			actions, skipped, err := service.ReconcileSandboxes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if skipped != 0 || catalog.record.State != test.wantState {
				t.Fatalf("actions=%+v skipped=%d state=%s steps=%v", actions, skipped, catalog.record.State, *steps)
			}
			if test.wantAction == "" && len(actions) != 0 || test.wantAction != "" && (len(actions) != 1 || actions[0].Action != test.wantAction) {
				t.Fatalf("actions = %+v, want %q", actions, test.wantAction)
			}
			if test.observation == vmm.ProcessRunning && strings.Contains(strings.Join(*steps, ","), "cleanup") {
				t.Fatalf("live VM was cleaned: %v", *steps)
			}
		})
	}
}

func TestReconcileSandboxesSkipsBusyOperation(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateCreating, Generation: 1,
	}
	path, err := service.dependencies.paths.Lock(fixedID)
	if err != nil {
		t.Fatal(err)
	}
	owner := filelock.New(path)
	if err := owner.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Unlock(t.Context()) }()
	actions, skipped, err := service.ReconcileSandboxes(t.Context())
	if err != nil || skipped != 1 || len(actions) != 0 || catalog.record.State != types.SandboxStateCreating || strings.Join(*steps, ",") != "list" {
		t.Fatalf("actions=%+v skipped=%d state=%s steps=%v error=%v", actions, skipped, catalog.record.State, *steps, err)
	}
}

func TestReconcileSandboxesFinishesOwnerlessCreate(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateCreating, Generation: 1,
	}
	actions, skipped, err := service.ReconcileSandboxes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(actions) != 1 || actions[0].Action != "removed-stale-create" || !catalog.deleted {
		t.Fatalf("actions=%+v skipped=%d deleted=%t steps=%v", actions, skipped, catalog.deleted, *steps)
	}
}

type recoveringRuntime struct {
	*fakeRuntime
	recover func(context.Context, types.SandboxID, uint64) (bool, error)
}

func (r *recoveringRuntime) RecoverCapture(ctx context.Context, id types.SandboxID, generation uint64) (bool, error) {
	return r.recover(ctx, id, generation)
}

func TestStartAndReconcileRecoverCaptureUnderSandboxLock(t *testing.T) {
	for _, operation := range []string{"start", "reconcile"} {
		t.Run(operation, func(t *testing.T) {
			service, _ := newTestSandboxService(t, nil)
			catalog := service.dependencies.catalog.(*fakeCatalog)
			catalog.record = types.Sandbox{ID: fixedID, VMM: types.VMMCloudHypervisor, Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4}
			runtime := testRuntime(t, service)
			runtime.observation = vmm.Observation{State: vmm.ProcessPaused}
			called := false
			registry, err := vmm.NewRegistry(&recoveringRuntime{fakeRuntime: runtime, recover: func(_ context.Context, id types.SandboxID, generation uint64) (bool, error) {
				if id != fixedID || generation != 3 {
					t.Fatalf("recovery identity = %s/%d", id, generation)
				}
				lockPath, err := service.dependencies.paths.Lock(id)
				if err != nil {
					t.Fatal(err)
				}
				lock := filelock.New(lockPath)
				acquired, err := lock.TryLock(t.Context())
				if acquired {
					_ = lock.Unlock(t.Context())
					t.Fatal("recovery ran without the operation lock")
				}
				if err != nil {
					t.Fatal(err)
				}
				called = true
				runtime.observation.State = vmm.ProcessRunning
				return true, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			service.dependencies.runtimes = registry
			if operation == "start" {
				_, err = service.Start(t.Context(), "box")
			} else {
				_, _, err = service.ReconcileSandboxes(t.Context())
			}
			if err != nil || !called || catalog.record.State != types.SandboxStateRunning {
				t.Fatalf("recovery called=%t state=%s error=%v", called, catalog.record.State, err)
			}
		})
	}
}

func TestStartPreservesPausedGuestWithoutCaptureIntent(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{ID: fixedID, VMM: types.VMMCloudHypervisor, Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4}
	testRuntime(t, service).observation = vmm.Observation{State: vmm.ProcessPaused}
	if _, err := service.Start(t.Context(), "box"); err == nil {
		t.Fatal("unowned pause accepted")
	}
	if strings.Contains(strings.Join(*steps, ","), "cleanup") || catalog.record.Generation != 4 {
		t.Fatalf("paused guest changed: %v", *steps)
	}
}
