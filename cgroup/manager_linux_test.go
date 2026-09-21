//go:build linux

package cgroup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kumabox/kumabox/types"
)

const testID = types.SandboxID("123e4567-e89b-42d3-a456-426614174000")

func TestWriteCPULimitsConvergesExistingScope(t *testing.T) {
	directory := t.TempDir()
	for _, cpus := range []uint32{2, 20_000} {
		if err := writeCPULimits(directory, cpus); err != nil {
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
