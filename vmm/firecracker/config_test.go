package firecracker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func testPlan(t *testing.T) vmm.LaunchPlan {
	t.Helper()
	args, err := vmm.OverlayV1Cmdline(vmm.OverlayV1Config{LayerCount: 2, Hostname: "fc-test"})
	if err != nil {
		t.Fatal(err)
	}
	return vmm.LaunchPlan{SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3, CPUs: 2, Memory: 1 << 30, BootProfile: types.BootProfileOverlayV1, Kernel: "/image/kernel", Initrd: "/image/initrd", Cmdline: args, Disks: []vmm.Disk{{Path: "/image/layer0", Serial: "kumabox-layer0", ReadOnly: true}, {Path: "/image/layer1", Serial: "kumabox-layer1", ReadOnly: true}, {Path: "/sandbox/cow.raw", Serial: vmm.COWSerial}}}
}

func TestConfigPreservesDiskOrderAndOverlayABI(t *testing.T) {
	plan := testPlan(t)
	cfg, err := buildConfig(plan, plan.Kernel, "/run/vsock")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Boot.Args, "kumabox.layers=/dev/vdb,/dev/vda kumabox.cow=/dev/vdc") || !strings.Contains(cfg.Boot.Args, "console=ttyS0") || strings.Contains(cfg.Boot.Args, "console=hvc0") {
		t.Fatalf("cmdline = %s", cfg.Boot.Args)
	}
	for i, d := range cfg.Drives {
		if d.Path != plan.Disks[i].Path || d.ReadOnly != plan.Disks[i].ReadOnly || d.Root {
			t.Fatalf("disk %d = %+v", i, d)
		}
	}
	if cfg.Balloon == nil || cfg.Balloon.AmountMiB != 256 || cfg.Machine.MemoryMiB != 1024 {
		t.Fatalf("memory config: %+v", cfg)
	}
	plan.NoBalloon = true
	cfg, err = buildConfig(plan, plan.Kernel, "/run/vsock")
	if err != nil || cfg.Balloon != nil {
		t.Fatalf("no-balloon = %+v, %v", cfg, err)
	}
}

func TestRejectsUnsupportedShapesBeforeLaunch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*vmm.LaunchPlan)
	}{
		{"shared", func(p *vmm.LaunchPlan) { p.SharedMemory = true }},
		{"hugepages", func(p *vmm.LaunchPlan) { p.HugePages = true }},
		{"KSM", func(p *vmm.LaunchPlan) { p.Mergeable = true }},
		{"memory alignment", func(p *vmm.LaunchPlan) { p.Memory++ }},
		{"vcpus", func(p *vmm.LaunchPlan) { p.CPUs = 33 }},
		{"direct IO", func(p *vmm.LaunchPlan) { enabled := true; p.Disks[2].DirectIO = &enabled }},
		{"MMIO limit", func(p *vmm.LaunchPlan) {
			for i := range 20 {
				p.Disks = append(p.Disks, vmm.Disk{Path: fmt.Sprintf("/data/%d", i), Serial: fmt.Sprintf("data%d", i)})
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := testPlan(t)
			test.mutate(&plan)
			if _, err := buildConfig(plan, plan.Kernel, "/run/vsock"); err == nil {
				t.Fatal("accepted incompatible launch shape")
			}
		})
	}
}

func TestDevicePathBeyondSingleLetter(t *testing.T) {
	for index, want := range map[int]string{0: "/dev/vda", 25: "/dev/vdz", 26: "/dev/vdaa", 51: "/dev/vdaz", 52: "/dev/vdba"} {
		if got := devicePath(index); got != want {
			t.Fatalf("device %d = %s", index, got)
		}
	}
}
