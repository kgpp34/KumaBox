package vmm

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/types"
)

func TestOverlayV1CmdlineListsLayersTopToBase(t *testing.T) {
	cmdline, err := OverlayV1Cmdline(OverlayV1Config{LayerCount: 3, Hostname: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdline, "boot=kumabox-overlay") || !strings.Contains(cmdline, "kumabox.layers=kumabox-layer2,kumabox-layer1,kumabox-layer0") || !strings.Contains(cmdline, "kumabox.cow=kumabox-cow") || !strings.Contains(cmdline, "kumabox.hostname=demo") {
		t.Fatalf("cmdline = %q", cmdline)
	}
}

func TestLaunchPlanRequiresBaseToTopReadOnlyLayersAndFinalCOW(t *testing.T) {
	plan := LaunchPlan{
		SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3,
		CPUs: 2, Memory: 1 << 30, BootProfile: types.BootProfileOverlayV1,
		Kernel: "/images/vmlinuz", Initrd: "/images/initrd.img", Cmdline: "boot=kumabox-overlay",
		Disks: []Disk{
			{Path: "/images/base.erofs", Serial: "kumabox-layer0", ReadOnly: true},
			{Path: "/images/top.erofs", Serial: "kumabox-layer1", ReadOnly: true},
			{Path: "/sandboxes/cow.raw", Serial: COWSerial},
		},
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	plan.Disks[1].ReadOnly = false
	if err := plan.Validate(); err == nil {
		t.Fatal("accepted a writable shared image layer")
	}
}

func TestProcessValidationRequiresCompleteIdentity(t *testing.T) {
	valid := Process{
		PID: 42, StartTicks: 100, BootID: "host-boot",
		SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3,
		Binary: "cloud-hypervisor", APISocket: "/run/kumabox/api.sock",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Process)
	}{
		{name: "PID", mutate: func(process *Process) { process.PID = 0 }},
		{name: "start time", mutate: func(process *Process) { process.StartTicks = 0 }},
		{name: "boot ID", mutate: func(process *Process) { process.BootID = "" }},
		{name: "sandbox ID", mutate: func(process *Process) { process.SandboxID = types.SandboxID("broken") }},
		{name: "generation", mutate: func(process *Process) { process.Generation = 0 }},
		{name: "binary", mutate: func(process *Process) { process.Binary = "" }},
		{name: "API socket", mutate: func(process *Process) { process.APISocket = "relative.sock" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("accepted incomplete process identity: %+v", candidate)
			}
		})
	}
}
