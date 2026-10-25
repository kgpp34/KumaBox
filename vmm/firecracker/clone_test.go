package firecracker

import (
	"os"
	"slices"
	"testing"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestValidateCloneDoesNotRequireSourceDisksOrRoots(t *testing.T) {
	f := testSnapshot(t)
	for _, disk := range f.meta.Config.Drives {
		if err := os.Remove(disk.Path); err != nil {
			t.Fatal(err)
		}
	}
	f.meta.Config.Vsock.Path = "/missing/source/run/vsock.uds"
	f.meta.Config.Boot.Kernel = "/missing/source/data/vmlinux"
	f.writeMeta(t)
	target := types.SandboxConfig{Name: "clone", CPUs: 2, Memory: 512 << 20, Storage: types.MinSandboxStorage}
	if err := f.driver.ValidateClone(t.Context(), f.directory, target, types.MemoryRestoreAuto); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*types.SandboxConfig)
	}{
		{name: "NIC resize", change: func(c *types.SandboxConfig) { c.NICs = 1; c.NetworkName = "test" }},
		{name: "extra disk", change: func(c *types.SandboxConfig) {
			c.DataDisks = []types.DataDiskSpec{{Name: "extra", Size: types.MinDataDiskSize, FSType: "none"}}
		}},
		{name: "RAM resize", change: func(c *types.SandboxConfig) { c.Memory = 1 << 30 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := target
			test.change(&config)
			if err := f.driver.ValidateClone(t.Context(), f.directory, config, types.MemoryRestoreAuto); err == nil {
				t.Fatal("accepted incompatible MMIO clone")
			}
		})
	}
}

func TestCloneBindingsUseDeviceIdentityInsteadOfAPIOrder(t *testing.T) {
	f := testSnapshot(t)
	meta := f.meta
	meta.Config.Drives = append(meta.Config.Drives, drive{ID: "disk2", Path: "/source/data.raw"})
	meta.Writable = append(meta.Writable, capturedDisk{Source: "/source/data.raw", File: "data-data.raw"})
	plan := vmm.ClonePlan{WritableDisk: "/target/cow.raw", ImageDisks: []vmm.Disk{{Path: "/target/layer.erofs", ReadOnly: true}}, DataDisks: []vmm.Disk{{Path: "/target/data.raw", Serial: "data"}}, RestorePlan: vmm.RestorePlan{CPUs: 2}}
	slices.Reverse(meta.Config.Drives)
	bindings, err := cloneBindings(meta, plan)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		got[binding.id] = binding.target
	}
	if got["disk0"] != "/target/layer.erofs" || got["disk1"] != "/target/cow.raw" || got["disk2"] != "/target/data.raw" {
		t.Fatalf("bindings = %v", got)
	}
	plan.NewDataDisks = []vmm.Disk{{Path: "/target/new.raw", Serial: "new"}}
	if _, err := cloneBindings(meta, plan); err == nil {
		t.Fatal("accepted extra MMIO disk")
	}
}

func TestCloneAliasesRejectConflictsAndTraversal(t *testing.T) {
	for _, test := range []struct {
		name  string
		disks []cloneDiskBinding
	}{
		{name: "conflicting source", disks: []cloneDiskBinding{{source: "/source/cow", target: "/one"}, {source: "/source/cow", target: "/two"}}},
		{name: "writable alias over image", disks: []cloneDiskBinding{{source: "/source/layer", target: "/image", readOnly: true}, {source: "/image", target: "/cow"}}},
		{name: "parent traversal", disks: []cloneDiskBinding{{source: "/source/../cow", target: "/target"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := cloneAliases(test.disks); err == nil {
				t.Fatal("accepted ambiguous alias")
			}
		})
	}
	aliases, err := cloneAliases([]cloneDiskBinding{{source: "/gone/source/cow", target: "/target/cow"}})
	if err != nil || len(aliases) != 2 {
		t.Fatalf("source-free alias map = %v %v", aliases, err)
	}
}
