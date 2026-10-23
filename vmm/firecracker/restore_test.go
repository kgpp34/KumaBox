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
		{name: "MAC", change: func(_ *snapshotConfig, p *vmm.RestorePlan) { p.Network.Interfaces[0].MAC = "02:00:00:00:00:99" }},
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
