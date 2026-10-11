//go:build !linux

package cgroup

import (
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

func TestManagerOperationsRequireLinux(t *testing.T) {
	manager, err := New(DefaultParent)
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	_, prepareErr := manager.Prepare(t.Context(), id, 2)
	_, pidsErr := manager.PIDs(id)
	removeErr := manager.Remove(t.Context(), id)
	for operation, err := range map[string]error{"Prepare": prepareErr, "PIDs": pidsErr, "Remove": removeErr} {
		if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeHostIncompatible {
			t.Fatalf("%s error = %v", operation, err)
		}
	}
}
