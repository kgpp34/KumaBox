package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

type balloonRuntime struct {
	*fakeRuntime
	resize func(context.Context, vmm.Process, types.BalloonTarget) (types.BalloonStatus, error)
}

func (r *balloonRuntime) ResizeBalloon(ctx context.Context, process vmm.Process, target types.BalloonTarget) (types.BalloonStatus, error) {
	return r.resize(ctx, process, target)
}

func TestResizeBalloonPreservesResourceShapeAndPropagatesObservation(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor, State: types.SandboxStateRunning, Generation: 4,
		Config: types.SandboxConfig{Name: "box", Memory: 1 << 30},
	}
	before := catalog.record
	process := vmm.Process{SandboxID: fixedID, Generation: 3, PID: 42}
	runtime := &balloonRuntime{fakeRuntime: testRuntime(t, service)}
	runtime.observation = vmm.Observation{State: vmm.ProcessRunning, Process: process}
	actual := int64(128 << 20)
	want := types.BalloonStatus{Target: 512 << 20, Actual: &actual, Default: 256 << 20}
	runtime.resize = func(_ context.Context, got vmm.Process, target types.BalloonTarget) (types.BalloonStatus, error) {
		if got != process || target.Size != want.Target {
			t.Fatalf("resize input = %+v, %+v", got, target)
		}
		runtime.observation.Balloon = &want
		return want, nil
	}
	var err error
	service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.ResizeBalloon(t.Context(), "box", types.BalloonTarget{Size: want.Target})
	if err != nil || !reflect.DeepEqual(status, want) || !reflect.DeepEqual(catalog.record, before) {
		t.Fatalf("resize = %+v, %v, durable state = %+v", status, err, catalog.record)
	}
	statuses, err := service.Status(t.Context(), "box")
	if err != nil || len(statuses) != 1 || !reflect.DeepEqual(statuses[0].Balloon, &want) {
		t.Fatalf("status = %+v, %v", statuses, err)
	}
}

func TestResizeBalloonRejectsInvalidRuntimeAndReleasesLock(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     types.SandboxState
		missing   bool
		supported bool
		fail      bool
		target    int64
		code      errdefs.Code
	}{
		{name: "stopped", state: types.SandboxStateStopped, code: errdefs.CodeStateConflict},
		{name: "unsupported", state: types.SandboxStateRunning, code: errdefs.CodeHostIncompatible},
		{name: "missing", state: types.SandboxStateRunning, supported: true, missing: true, code: errdefs.CodeArtifactUnavailable},
		{name: "invalid size", state: types.SandboxStateRunning, target: 1 << 30, code: errdefs.CodeInvalidArgument},
		{name: "VMM failure", state: types.SandboxStateRunning, supported: true, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := newTestSandboxService(t, nil)
			catalog := service.dependencies.catalog.(*fakeCatalog)
			catalog.record = types.Sandbox{
				ID: fixedID, VMM: types.VMMCloudHypervisor, State: test.state, Generation: 4,
				Config: types.SandboxConfig{Name: "box", Memory: 1 << 30},
			}
			runtime := testRuntime(t, service)
			if !test.missing {
				runtime.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{SandboxID: fixedID, Generation: 3}}
			}
			failure := errors.New("injected resize failure")
			if test.supported {
				var err error
				service.dependencies.runtimes, err = vmm.NewRegistry(&balloonRuntime{
					fakeRuntime: runtime,
					resize: func(context.Context, vmm.Process, types.BalloonTarget) (types.BalloonStatus, error) {
						if !test.fail {
							t.Fatal("invalid request reached VMM")
						}
						return types.BalloonStatus{}, failure
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				ctx, cancel := context.WithTimeout(t.Context(), service.dependencies.cleanupTimeout)
				_, err := service.ResizeBalloon(ctx, "box", types.BalloonTarget{Size: test.target})
				cancel()
				if test.fail {
					if !errors.Is(err, failure) {
						t.Fatalf("resize error = %v", err)
					}
				} else if code, _ := errdefs.CodeOf(err); code != test.code {
					t.Fatalf("resize error = %v, code = %s", err, code)
				}
			}
		})
	}
}
