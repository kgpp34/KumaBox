package firecracker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// ValidateConfig refuses unsupported options before core reserves a name or disk.
func (*Driver) ValidateConfig(config types.SandboxConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	return validateShape(config)
}

func validateShape(config types.SandboxConfig) error {
	if config.SharedMemory || config.HugePages || config.Mergeable {
		return invalid(errors.New("firecracker does not support shared-memory, hugepages or mergeable memory in this backend"))
	}
	if config.CPUs > 32 || config.Memory%(1<<20) != 0 {
		return invalid(errors.New("firecracker requires at most 32 vCPUs and memory aligned to MiB"))
	}
	for _, disk := range config.DataDisks {
		if disk.DirectIO != nil && *disk.DirectIO {
			return invalid(fmt.Errorf("firecracker does not support directio=on for disk %q", disk.Name))
		}
	}
	return nil
}

func invalid(err error) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
}

func buildConfig(plan vmm.LaunchPlan, kernel, socket string) (vmConfig, error) {
	if err := plan.Validate(); err != nil {
		return vmConfig{}, invalid(err)
	}
	if err := validateShape(types.SandboxConfig{CPUs: plan.CPUs, Memory: plan.Memory, SharedMemory: plan.SharedMemory, HugePages: plan.HugePages, Mergeable: plan.Mergeable}); err != nil {
		return vmConfig{}, err
	}
	// x86 MMIO has 17 device slots; reserve vsock, entropy and the balloon.
	devices := len(plan.Disks) + len(plan.Network.Interfaces) + 2
	if !plan.NoBalloon {
		devices++
	}
	if devices > 17 {
		return vmConfig{}, invalid(fmt.Errorf("firecracker MMIO device budget exceeded: %d devices, maximum 17", devices))
	}
	cfg := vmConfig{
		Machine: machineConfig{VCPUs: plan.CPUs, MemoryMiB: plan.Memory >> 20},
		Boot:    bootSource{Kernel: kernel, Initrd: plan.Initrd, Args: bootArgs(plan)},
		Vsock:   vsock{CID: vmm.VsockGuestCID, Path: socket},
		Drives:  make([]drive, 0, len(plan.Disks)),
		NICs:    make([]networkInterface, 0, len(plan.Network.Interfaces)),
	}
	for i, disk := range plan.Disks {
		if disk.DirectIO != nil && *disk.DirectIO {
			return vmConfig{}, invalid(fmt.Errorf("firecracker does not support directio=on for disk %q", disk.Serial))
		}
		cfg.Drives = append(cfg.Drives, drive{ID: fmt.Sprintf("disk%d", i), Path: disk.Path, ReadOnly: disk.ReadOnly, Cache: "Writeback", Engine: "Sync"})
	}
	for _, nic := range plan.Network.Interfaces {
		if nic.Queues != 2 {
			return vmConfig{}, invalid(errors.New("firecracker requires a single RX/TX queue pair per NIC"))
		}
		cfg.NICs = append(cfg.NICs, networkInterface{ID: fmt.Sprintf("eth%d", nic.Index), TAP: nic.TAP, MAC: nic.MAC})
	}
	if !plan.NoBalloon {
		cfg.Balloon = &balloon{AmountMiB: (plan.Memory / 4) >> 20, DeflateOnOOM: true}
	}
	return cfg, nil
}

// bootArgs changes only the transport-specific console and disk references.
// Overlay ordering, hostname and network parameters keep the shared boot ABI.
func bootArgs(plan vmm.LaunchPlan) string {
	args := strings.Fields(plan.Cmdline)
	mapping := make(map[string]string, len(plan.Disks))
	for index, disk := range plan.Disks {
		mapping[disk.Serial] = devicePath(index)
	}
	for i, arg := range args {
		switch {
		case arg == "console=hvc0":
			args[i] = "console=ttyS0 reboot=k i8042.noaux 8250.nr_uarts=1"
		case strings.HasPrefix(arg, "kumabox.layers="):
			layers := strings.Split(strings.TrimPrefix(arg, "kumabox.layers="), ",")
			for j, name := range layers {
				layers[j] = mapping[name]
			}
			args[i] = "kumabox.layers=" + strings.Join(layers, ",")
		case strings.HasPrefix(arg, "kumabox.cow="):
			args[i] = "kumabox.cow=" + mapping[vmm.COWSerial]
		}
	}
	return strings.Join(args, " ")
}

// devicePath follows Linux virtio-blk attachment order, including indices >=26.
func devicePath(index int) string {
	suffix := ""
	for n := index + 1; n > 0; n = (n - 1) / 26 {
		suffix = string(rune('a'+(n-1)%26)) + suffix
	}
	return "/dev/vd" + suffix
}
