//go:build !linux

package cgroup

import (
	"testing"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
)

func TestManagerOperationsRequireLinux(t *testing.T) {
	manager, err := New(DefaultParent)
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	_, prepareErr := manager.Prepare(t.Context(), id, 2, types.CPUPolicy{})
	_, restoreErr := manager.PrepareRestore(t.Context(), id, 2, types.CPUPolicy{})
	limitErr := manager.ApplyLimit(t.Context(), id, 2, types.CPUPolicy{})
	_, pidsErr := manager.PIDs(id)
	removeErr := manager.Remove(t.Context(), id)
	for operation, err := range map[string]error{"Prepare": prepareErr, "PrepareRestore": restoreErr, "ApplyLimit": limitErr, "PIDs": pidsErr, "Remove": removeErr} {
		if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeHostIncompatible {
			t.Fatalf("%s error = %v", operation, err)
		}
	}
}
