package core

import (
	"testing"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestStatusReportsDeadRunningVMWithoutChangingRecord(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4,
	}
	statuses, err := service.Status(t.Context(), "box", fixedID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].Stale || statuses[0].Runtime != vmm.ProcessAbsent {
		t.Fatalf("status = %+v", statuses)
	}
	if catalog.record.State != types.SandboxStateRunning || catalog.record.Generation != 4 {
		t.Fatalf("status mutated metadata: %+v", catalog.record)
	}
	for _, step := range *steps {
		if step == "stopped" || step == "cleanup" || step == "network-quiesce" {
			t.Fatalf("read-only status performed cleanup: %v", *steps)
		}
	}
}

func TestStatusLeavesCreatedVMWithoutRuntimeProbe(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	service.dependencies.catalog.(*fakeCatalog).record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateCreated, Generation: 2,
	}
	statuses, err := service.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Runtime != "" || statuses[0].Stale {
		t.Fatalf("status = %+v", statuses)
	}
	for _, step := range *steps {
		if step == "observe" {
			t.Fatalf("created sandbox was probed: %v", *steps)
		}
	}
}
