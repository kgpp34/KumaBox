package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const (
	externalDiskPrefix = "kumabox-disk-"
	deviceEjectTimeout = 30 * time.Second
)

var (
	_ vmm.DiskHotplugger = (*Driver)(nil)
	_ vmm.PCIHotplugger  = (*Driver)(nil)
)

func diskID(name string) string { return externalDiskPrefix + name }

// AttachedDisks reads the current VMM configuration, excluding sandbox-owned
// boot disks. Runtime attachments do not survive a new VMM process.
func (d *Driver) AttachedDisks(ctx context.Context, process vmm.Process) ([]types.AttachedDisk, error) {
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return nil, err
	}
	return runtimeDisks(info), nil
}

func runtimeDisks(info vmInfo) []types.AttachedDisk {
	var disks []types.AttachedDisk
	for _, disk := range info.Config.Disks {
		name, found := strings.CutPrefix(disk.ID, externalDiskPrefix)
		if !found || types.ValidateExternalDiskName(name) != nil {
			continue
		}
		disks = append(disks, types.AttachedDisk{ID: disk.ID, Name: name, Path: disk.Path, ReadOnly: disk.ReadOnly})
	}
	return disks
}

// AddDisk uses a deterministic ID so an interrupted API call can be checked
// against vm.info before the caller decides whether the attach succeeded.
func (d *Driver) AddDisk(ctx context.Context, process vmm.Process, spec types.ExternalDisk) error {
	if err := types.ValidateExternalDiskName(spec.Name); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return err
	}
	for _, disk := range info.Config.Disks {
		if disk.ID == diskID(spec.Name) || disk.Serial == spec.Name || disk.Path == spec.Path {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("disk name or path is already attached: %s", spec.Name))
		}
	}
	payload, err := json.Marshal(map[string]any{
		"id": diskID(spec.Name), "path": spec.Path, "serial": spec.Name,
		"image_type": "Raw", "readonly": spec.ReadOnly, "direct": spec.DirectIO,
		"sparse": !spec.ReadOnly, "num_queues": spec.Queues, "queue_size": diskQueueSize,
	})
	if err != nil {
		return err
	}
	requestErr := d.snapshotAction(ctx, process.APISocket, "vm.add-disk", payload, d.startupTimeout)
	if requestErr == nil {
		return nil
	}
	current, inspectErr := d.liveInfo(ctx, process)
	if inspectErr == nil {
		for _, disk := range runtimeDisks(current) {
			if disk.ID == diskID(spec.Name) && disk.Path == spec.Path {
				return nil
			}
		}
	}
	return errors.Join(requestErr, inspectErr)
}

// RemoveDisk removes only a runtime disk with the derived ID. It keeps the
// backing file and waits for the guest's PCI eject acknowledgment.
func (d *Driver) RemoveDisk(ctx context.Context, process vmm.Process, name string) error {
	if err := types.ValidateExternalDiskName(name); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return err
	}
	id := diskID(name)
	for _, disk := range info.Config.Disks {
		if disk.ID == id {
			return d.removeRuntimeDevice(ctx, process, id)
		}
	}
	return errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("disk %q is not attached", name))
}

// AttachedPCIDevices reports live VFIO devices from vm.info.
func (d *Driver) AttachedPCIDevices(ctx context.Context, process vmm.Process) ([]types.AttachedPCIDevice, error) {
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return nil, err
	}
	devices := make([]types.AttachedPCIDevice, 0, len(info.Config.Devices))
	for _, device := range info.Config.Devices {
		bdf, _ := strings.CutPrefix(device.Path, "/sys/bus/pci/devices/")
		devices = append(devices, types.AttachedPCIDevice{ID: device.ID, BDF: bdf})
	}
	return devices, nil
}

// AddPCIDevice gives Cloud Hypervisor a canonical sysfs path and an explicit
// detach key. The caller validates the host's VFIO binding before reaching here.
func (d *Driver) AddPCIDevice(ctx context.Context, process vmm.Process, spec types.PCIDevice) error {
	if err := types.ValidatePCIDeviceID(spec.ID); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return err
	}
	for _, device := range info.Config.Devices {
		if device.ID == spec.ID || device.Path == spec.Path {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("PCI device id or path is already attached: %s", spec.ID))
		}
	}
	payload, err := json.Marshal(map[string]string{"id": spec.ID, "path": spec.Path})
	if err != nil {
		return err
	}
	requestErr := d.snapshotAction(ctx, process.APISocket, "vm.add-device", payload, d.startupTimeout)
	if requestErr == nil {
		return nil
	}
	current, inspectErr := d.liveInfo(ctx, process)
	if inspectErr == nil {
		for _, device := range current.Config.Devices {
			if device.ID == spec.ID && device.Path == spec.Path {
				return nil
			}
		}
	}
	return errors.Join(requestErr, inspectErr)
}

// RemovePCIDevice waits until the device disappears from the VMM device tree.
func (d *Driver) RemovePCIDevice(ctx context.Context, process vmm.Process, id string) error {
	if err := types.ValidatePCIDeviceID(id); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return err
	}
	for _, device := range info.Config.Devices {
		if device.ID == id {
			return d.removeRuntimeDevice(ctx, process, id)
		}
	}
	return errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("PCI device %q is not attached", id))
}

func (d *Driver) removeRuntimeDevice(ctx context.Context, process vmm.Process, id string) error {
	payload, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return err
	}
	requestErr := d.snapshotAction(ctx, process.APISocket, "vm.remove-device", payload, d.startupTimeout)
	deadline := time.NewTimer(deviceEjectTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := d.liveInfo(ctx, process)
		if err != nil {
			return errors.Join(requestErr, err)
		}
		if _, present := info.DeviceTree[id]; !present {
			return nil
		}
		if requestErr != nil {
			return requestErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("guest has not ejected device %s; unmount or release it inside the guest and retry", id))
		case <-ticker.C:
		}
	}
}
