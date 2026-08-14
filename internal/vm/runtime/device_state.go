package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vm"
)

// RefreshDeviceState reconciles durable hotplug metadata with one live
// vm.info response while holding the VM operation lock.
func (r *Runtime) RefreshDeviceState(ctx context.Context, ref string) (*vm.VMRecord, error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for device inspection: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	inspector, ok := r.backend.(backend.DeviceInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not inspect devices")
	}
	live, err := inspector.InspectDevices(ctx, rec)
	if err != nil {
		return nil, err
	}
	updated, err := r.vmRecords.SetAttachedDisks(rec.ID, toVMDisks(live.Disks))
	if err != nil {
		return nil, err
	}
	updated, err = r.vmRecords.SetAttachedFilesystems(updated.ID, toVMFilesystems(live.Filesystems))
	if err != nil {
		return nil, err
	}
	updated, err = r.vmRecords.SetAttachedPCIDevices(updated.ID, toVMPCIDevices(live.PCIDevices))
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func toVMDisks(items []backend.AttachedDisk) []vm.AttachedDisk {
	result := make([]vm.AttachedDisk, 0, len(items))
	for _, item := range items {
		result = append(result, vm.AttachedDisk{ID: item.ID, Name: item.Name, Path: item.Path, ReadOnly: item.ReadOnly})
	}
	return result
}

func toVMFilesystems(items []backend.AttachedFilesystem) []vm.AttachedFilesystem {
	result := make([]vm.AttachedFilesystem, 0, len(items))
	for _, item := range items {
		result = append(result, vm.AttachedFilesystem{ID: item.ID, Tag: item.Tag, Socket: item.Socket})
	}
	return result
}

func toVMPCIDevices(items []backend.AttachedPCIDevice) []vm.AttachedPCIDevice {
	result := make([]vm.AttachedPCIDevice, 0, len(items))
	for _, item := range items {
		result = append(result, vm.AttachedPCIDevice{ID: item.ID, PCI: item.PCI})
	}
	return result
}
