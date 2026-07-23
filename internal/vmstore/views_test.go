package vmstore

import (
	"testing"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

func TestRecordViewsSeparateDesiredAndRuntimeData(t *testing.T) {
	record := &VMRecord{
		ID: "vm-1", Name: "agent", Backend: "cloud-hypervisor", State: StateRunning,
		ObservedState: ObservedStateRunning, RootDisk: "/disk", CPUs: 2, MemoryBytes: 1 << 30,
		Image:          &ImageRef{ID: "img-1", LayerDigests: []string{"sha256:a"}},
		NetworkConfigs: []kbnetwork.Config{{ID: "net-1"}},
	}
	config := record.ConfigView()
	runtime := record.RuntimeView()
	attachments := record.AttachmentsView()
	refs := record.ReferencesView()

	record.State = StateStopped
	record.ObservedState = ObservedStateStopped
	record.Image.LayerDigests[0] = "sha256:changed"
	record.NetworkConfigs[0].ID = "changed"

	if config.CPUs != 2 || config.MemoryBytes != 1<<30 || config.Image.LayerDigests[0] != "sha256:a" {
		t.Fatalf("config view changed with runtime record mutation: %+v", config)
	}
	if runtime.Desired != StateRunning || runtime.Observed != ObservedStateRunning {
		t.Fatalf("runtime view = %+v", runtime)
	}
	if len(attachments.NetworkConfigs) != 1 || attachments.NetworkConfigs[0].ID != "net-1" {
		t.Fatalf("attachments view = %+v", attachments)
	}
	if refs.ImageID != "img-1" {
		t.Fatalf("references view = %+v", refs)
	}
}
