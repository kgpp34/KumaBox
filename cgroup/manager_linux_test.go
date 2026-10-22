//go:build linux

package cgroup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/types"
)

const testID = types.SandboxID("123e4567-e89b-42d3-a456-426614174000")

func TestWriteCPULimitsConvergesExistingScope(t *testing.T) {
	directory := t.TempDir()
	// Cgroup control files already exist in the kernel-backed scope.
	for _, name := range []string{"cpu.weight", "cpu.max"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, cpus := range []uint32{2, 20_000} {
		if err := writeCPULimits(directory, cpus, types.CPUPolicy{}, false); err != nil {
			t.Fatal(err)
		}
		weight, err := os.ReadFile(filepath.Join(directory, "cpu.weight"))
		if err != nil {
			t.Fatal(err)
		}
		maximum, err := os.ReadFile(filepath.Join(directory, "cpu.max"))
		if err != nil {
			t.Fatal(err)
		}
		wantWeight, wantMaximum := "2", "200000 100000"
		if cpus == 20_000 {
			wantWeight, wantMaximum = "10000", "2000000000 100000"
		}
		if string(weight) != wantWeight || string(maximum) != wantMaximum {
			t.Fatalf("CPUs %d: weight=%q max=%q", cpus, weight, maximum)
		}
	}
}

func TestApplyLimitConvergesProvisioningQuotaAndHonorsCancellation(t *testing.T) {
	manager := &Manager{parent: t.TempDir()}
	directory, err := manager.scopeDir(testID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"cpu.max": "max 100000", "cpu.weight": "1"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCPULimits(directory, 2, types.CPUPolicy{}, true); err != nil {
		t.Fatal(err)
	}
	provisioning, err := os.ReadFile(filepath.Join(directory, "cpu.max"))
	if err != nil || string(provisioning) != "max 100000" {
		t.Fatalf("provisioning limit = %q, %v", provisioning, err)
	}
	if err := manager.ApplyLimit(t.Context(), testID, 2, types.CPUPolicy{}); err != nil {
		t.Fatal(err)
	}
	maximum, err := os.ReadFile(filepath.Join(directory, "cpu.max"))
	if err != nil || string(maximum) != "200000 100000" {
		t.Fatalf("runtime limit = %q, %v", maximum, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.ApplyLimit(ctx, testID, 1, types.CPUPolicy{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled limit = %v", err)
	}
	if err := manager.ApplyLimit(t.Context(), testID, 0, types.CPUPolicy{}); err == nil {
		t.Fatal("accepted zero CPUs")
	}
}

func TestPIDsSortsCompactsAndRemoveReclaimsEmptyScope(t *testing.T) {
	manager := &Manager{parent: t.TempDir()}
	directory, err := manager.scopeDir(testID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	processFile := filepath.Join(directory, "cgroup.procs")
	if err := os.WriteFile(processFile, []byte("42\n7\n42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pids, err := manager.PIDs(testID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{7, 42}) {
		t.Fatalf("PIDs() = %v", pids)
	}
	if err := os.Remove(processFile); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(t.Context(), testID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scope remains after remove: %v", err)
	}
}

func TestRemoveBusyScopeHonorsCancellation(t *testing.T) {
	manager := &Manager{parent: t.TempDir()}
	directory, err := manager.scopeDir(testID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "busy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.Remove(ctx, testID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove() error = %v", err)
	}
}

func TestCPUControlsRestoreAndConvergeCustomPolicy(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cpu.weight", "cpu.max", "cpu.max.burst"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("200000"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := types.CPUPolicy{Weight: 37, QuotaUS: 75000, PeriodUS: 50000, BurstUS: 10000}
	for _, restoring := range []bool{true, false} {
		if err := writeCPULimits(dir, 2, policy, restoring); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"cpu.weight": "37", "cpu.max": "75000 50000", "cpu.max.burst": "10000"}
		if restoring {
			want["cpu.max"], want["cpu.max.burst"] = "max 50000", "0"
		}
		for name, value := range want {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || string(got) != value {
				t.Fatalf("restoring=%t %s=%q, %v; want %q", restoring, name, got, err, value)
			}
		}
	}
	if err := writeCPULimits(dir, 2, types.CPUPolicy{BurstUS: -1}, false); err != nil {
		t.Fatal(err)
	}
	burst, err := os.ReadFile(filepath.Join(dir, "cpu.max.burst"))
	if err != nil || string(burst) != "0" {
		t.Fatalf("disabled burst=%q, %v", burst, err)
	}
	if err := os.Remove(filepath.Join(dir, "cpu.max.burst")); err != nil {
		t.Fatal(err)
	}
	if err := writeCPULimits(dir, 2, types.CPUPolicy{BurstUS: 10000}, false); err == nil {
		t.Fatal("explicit burst silently ignored on unsupported kernel")
	}
	if err := writeCPULimits(dir, 2, types.CPUPolicy{BurstUS: -1}, false); err != nil {
		t.Fatalf("disabling burst should work without kernel burst support: %v", err)
	}
	if err := writeCPULimits(dir, 2, types.CPUPolicy{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestCPUPlacementChecksEffectiveSetAndClearsReuse(t *testing.T) {
	parent, dir := t.TempDir(), t.TempDir()
	for path, value := range map[string]string{
		filepath.Join(parent, "cpuset.cpus.effective"): "0-7",
		filepath.Join(dir, "cpuset.cpus.effective"):    "2-3",
		filepath.Join(dir, "cpuset.cpus"):              "",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := placeCPUs(parent, dir, "2-3"); err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []string{"2-4", "8", "0-4294967295"} {
		if err := placeCPUs(parent, dir, unavailable); err == nil {
			t.Fatalf("accepted unavailable cpuset %s", unavailable)
		}
	}
	if err := placeCPUs(parent, dir, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "cpuset.cpus"))
	if err != nil || strings.TrimSpace(string(got)) != "" {
		t.Fatalf("reused placement=%q, %v", got, err)
	}
}

// This opt-in test needs a delegated Linux cgroup v2 hierarchy. Ordinary unit
// tests still run without root; integration runs must opt in explicitly.
func TestKernelCPUControlsConvergeReusedEmptyScope(t *testing.T) {
	if os.Getenv("KUMABOX_CGROUP_INTEGRATION") != "1" {
		t.Skip("requires writable cgroup v2")
	}
	id, err := types.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(Root, "kumabox-resource-test-"+id.String()+".slice")
	manager, err := New(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Remove(context.Background(), id); err != nil {
			t.Error(err)
		}
		if err := os.Remove(parent); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	available, err := os.ReadFile(filepath.Join(Root, "cpuset.cpus.effective"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(available)), ",")
	first, _, _ = strings.Cut(first, "-")
	scope, err := manager.Prepare(t.Context(), id, 2, types.CPUPolicy{CPUSet: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	// Lowering quota must first clear old burst credit. Empty placement must
	// undo the previous CPU assignment, rather than keep a silently pinned VM.
	scope, err = manager.Prepare(t.Context(), id, 2, types.CPUPolicy{QuotaUS: 75000})
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := manager.scopeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]string{"cpu.max": "75000 100000", "cpu.max.burst": "75000", "cpuset.cpus": ""} {
		raw, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || strings.TrimSpace(string(raw)) != expected {
			t.Fatalf("%s=%q, %v; want %q", name, raw, err, expected)
		}
	}
	inherited, err := os.ReadFile(filepath.Join(parent, "cpuset.cpus.effective"))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "cpuset.cpus.effective"))
	if err != nil || string(actual) != string(inherited) {
		t.Fatalf("effective placement=%q, %v; want %q", actual, err, inherited)
	}
}
