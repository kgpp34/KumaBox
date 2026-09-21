package cloudhypervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestNewUsesConfiguredLifecyclePolicy(t *testing.T) {
	base := t.TempDir()
	paths, err := vmm.NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := cgroup.New("/sys/fs/cgroup/kumabox-test.slice")
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Binary: "custom-vmm", StartupTimeout: 2 * time.Second,
		StopGrace: 3 * time.Second, AbortGrace: 4 * time.Second,
	}
	driver, err := New(paths, scopes, options)
	if err != nil {
		t.Fatal(err)
	}
	if driver.binary != options.Binary || driver.startupTimeout != options.StartupTimeout ||
		driver.stopGrace != options.StopGrace || driver.abortGrace != options.AbortGrace {
		t.Fatalf("driver policy = %+v", driver)
	}
}

func TestNewRejectsInvalidLifecyclePolicy(t *testing.T) {
	scopes, err := cgroup.New("/sys/fs/cgroup/kumabox-test.slice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(vmm.Paths{}, scopes, Options{StartupTimeout: time.Nanosecond}); err == nil {
		t.Fatal("New() accepted a startup timeout shorter than one probe")
	}
}

func TestWaitReadyHandlesProcessAndAPITransitions(t *testing.T) {
	process := vmm.Process{
		PID: 42, StartTicks: 100, BootID: "boot",
		SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3,
		Binary: "cloud-hypervisor", APISocket: "/run/api.sock",
	}
	failure := errors.New("observe failed")
	tests := []struct {
		name      string
		ctx       func() context.Context
		observe   func(context.Context, types.SandboxID, uint64) (vmm.Observation, error)
		wantError error
		wantCode  errdefs.Code
	}{
		{
			name: "running exact process",
			ctx:  t.Context,
			observe: func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
				return vmm.Observation{State: vmm.ProcessRunning, Process: process}, nil
			},
		},
		{
			name: "process exits before ready",
			ctx:  t.Context,
			observe: func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
				return vmm.Observation{State: vmm.ProcessAbsent}, nil
			},
			wantCode: errdefs.CodeArtifactUnavailable,
		},
		{
			name: "identity changes",
			ctx:  t.Context,
			observe: func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
				changed := process
				changed.StartTicks++
				return vmm.Observation{State: vmm.ProcessRunning, Process: changed}, nil
			},
			wantCode: errdefs.CodeStateConflict,
		},
		{
			name: "observation fails",
			ctx:  t.Context,
			observe: func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
				return vmm.Observation{}, failure
			},
			wantError: failure,
		},
		{
			name: "caller cancels while API is unavailable",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			observe: func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
				return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
			},
			wantError: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := waitReady(test.ctx(), process, time.Second, test.observe)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("waitReady error = %v, want %v", err, test.wantError)
				}
				return
			}
			if test.wantCode != "" {
				if code, ok := errdefs.CodeOf(err); !ok || code != test.wantCode {
					t.Fatalf("waitReady code = %q, %v; error = %v", code, ok, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitReadyTimesOutWhileAPIIsUnavailable(t *testing.T) {
	process := vmm.Process{SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3}
	err := waitReady(t.Context(), process, probeInterval, func(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
		return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
	})
	if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactUnavailable {
		t.Fatalf("timeout code = %q, %v; error = %v", code, ok, err)
	}
}

type cleanupScope struct {
	removeErr error
	removals  int
}

func (*cleanupScope) Prepare(context.Context, types.SandboxID, uint32) (*os.File, error) {
	return nil, errors.New("not used")
}
func (*cleanupScope) PIDs(types.SandboxID) ([]int, error) { return nil, nil }
func (s *cleanupScope) Remove(context.Context, types.SandboxID) error {
	s.removals++
	return s.removeErr
}

func TestCleanupRetainsRuntimeUntilCgroupRemovalCanRetry(t *testing.T) {
	base := t.TempDir()
	paths, err := vmm.NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	if err := paths.Prepare(id); err != nil {
		t.Fatal(err)
	}
	scopeFailure := errors.New("cgroup is still busy")
	scopes := &cleanupScope{removeErr: scopeFailure}
	driver := &Driver{paths: paths, scopes: scopes}
	if err := driver.Cleanup(t.Context(), id); !errors.Is(err, scopeFailure) {
		t.Fatalf("first cleanup error = %v", err)
	}
	runtimeDir, err := paths.RunDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Fatalf("runtime state was removed before cgroup cleanup: %v", err)
	}
	scopes.removeErr = nil
	if err := driver.Cleanup(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime state remains after retry: %v", err)
	}
	if scopes.removals != 2 {
		t.Fatalf("cgroup removals = %d, want 2", scopes.removals)
	}
}
