package firecracker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestValidateRestoreMemoryModes(t *testing.T) {
	f := testSnapshot(t)
	for _, mode := range []types.MemoryRestoreMode{"", types.MemoryRestoreAuto, types.MemoryRestoreMmap, types.MemoryRestoreOnDemand} {
		t.Run(string(mode), func(t *testing.T) {
			if err := f.driver.ValidateRestore(t.Context(), f.directory, mode); err != nil {
				t.Fatal(err)
			}
		})
	}
	err := f.driver.ValidateRestore(t.Context(), f.directory, types.MemoryRestoreCopy)
	if code, _ := errdefs.CodeOf(err); code != errdefs.CodeHostIncompatible {
		t.Fatalf("copy mode error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.driver.ValidateRestore(ctx, f.directory, types.MemoryRestoreAuto); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation = %v", err)
	}
}

func TestValidateRestoreRefusesRelocatedRoots(t *testing.T) {
	for _, which := range []string{"data", "runtime"} {
		t.Run(which, func(t *testing.T) {
			f := testSnapshot(t)
			if which == "data" {
				f.meta.Config.Boot.Kernel = filepath.Join(t.TempDir(), "vmlinux")
			} else {
				f.meta.Config.Vsock.Path = filepath.Join(t.TempDir(), "vsock.uds")
			}
			f.writeMeta(t)
			if err := f.driver.ValidateSnapshot(t.Context(), f.directory); err != nil {
				t.Fatal(err)
			}
			err := f.driver.ValidateRestore(t.Context(), f.directory, types.MemoryRestoreAuto)
			if code, _ := errdefs.CodeOf(err); code != errdefs.CodeHostIncompatible {
				t.Fatalf("relocated %s root = %v", which, err)
			}
		})
	}
}

func TestRestoreBindingsKeepOwnerAndNetworkIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*snapshotConfig, *vmm.RestorePlan)
	}{
		{name: "owner", change: func(_ *snapshotConfig, p *vmm.RestorePlan) { p.SandboxID = "00000000-0000-4000-8000-000000000000" }},
		{name: "CPUs", change: func(_ *snapshotConfig, p *vmm.RestorePlan) { p.CPUs = 1 }},
		{name: "NIC count", change: func(_ *snapshotConfig, p *vmm.RestorePlan) { p.Network.Interfaces = nil }},
		{name: "TAP", change: func(_ *snapshotConfig, p *vmm.RestorePlan) { p.Network.Interfaces[0].TAP = "foreign" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := testSnapshot(t)
			f.meta.Config.NICs = []networkInterface{{ID: "eth0", TAP: "tap-source", MAC: "02:00:00:00:00:01"}}
			plan := vmm.RestorePlan{SandboxID: f.meta.SandboxID, CPUs: 2, Network: types.NetworkSetup{Interfaces: []types.NetworkInterface{{Index: 0, TAP: "tap-source", MAC: "02:00:00:00:00:01"}}}}
			if err := validateRestoreBindings(f.meta, plan); err != nil {
				t.Fatal(err)
			}
			test.change(&f.meta, &plan)
			if err := validateRestoreBindings(f.meta, plan); err == nil {
				t.Fatal("accepted foreign bindings")
			}
		})
	}
}

// A clone's boot source can still name the original kernel cache. Version two
// binds restore to the current owner roots instead of inheriting that fence.
func TestValidateRestoreUsesCurrentOwnerAfterCrossRootClone(t *testing.T) {
	f := testSnapshot(t)
	roots := f.driver.paths.Roots()
	f.meta.Version = 2
	f.meta.Owner = &snapshotOwner{Data: roots.Data, Run: roots.Run}
	f.meta.Config.Boot.Kernel = "/gone/original/data/vmlinux"
	f.writeMeta(t)
	if err := f.driver.ValidateRestore(t.Context(), f.directory, types.MemoryRestoreMmap); err != nil {
		t.Fatal(err)
	}
	f.meta.Owner.Run = "/foreign/runtime"
	f.writeMeta(t)
	if err := f.driver.ValidateRestore(t.Context(), f.directory, types.MemoryRestoreMmap); err == nil {
		t.Fatal("accepted another owner's runtime root")
	}
}

func TestRestoreBindingsAllowGuestMACChangeAndUnorderedNICs(t *testing.T) {
	meta := snapshotConfig{SandboxID: "123e4567-e89b-42d3-a456-426614174000"}
	meta.Config.Machine.VCPUs = 2
	meta.Config.NICs = []networkInterface{
		{ID: "eth1", TAP: "tap-one", MAC: "02:00:00:00:00:01"},
		{ID: "eth0", TAP: "tap-zero", MAC: "02:00:00:00:00:02"},
	}
	plan := vmm.RestorePlan{SandboxID: meta.SandboxID, CPUs: 2, Network: types.NetworkSetup{Interfaces: []types.NetworkInterface{
		{Index: 0, TAP: "tap-zero", MAC: "02:00:00:00:00:03"},
		{Index: 1, TAP: "tap-one", MAC: "02:00:00:00:00:04"},
	}}}
	if err := validateRestoreBindings(meta, plan); err != nil {
		t.Fatal(err)
	}
}
