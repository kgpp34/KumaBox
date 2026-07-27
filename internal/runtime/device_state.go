package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// RefreshDeviceState reconciles durable hotplug metadata with one live
// vm.info response while holding the VM operation lock.
func (r *Runtime) RefreshDeviceState(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
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

func toVMDisks(items []backend.AttachedDisk) []vmstore.AttachedDisk {
	result := make([]vmstore.AttachedDisk, 0, len(items))
	for _, item := range items {
		result = append(result, vmstore.AttachedDisk{ID: item.ID, Name: item.Name, Path: item.Path, ReadOnly: item.ReadOnly})
	}
	return result
}

func toVMFilesystems(items []backend.AttachedFilesystem) []vmstore.AttachedFilesystem {
	result := make([]vmstore.AttachedFilesystem, 0, len(items))
	for _, item := range items {
		result = append(result, vmstore.AttachedFilesystem{ID: item.ID, Tag: item.Tag, Socket: item.Socket})
	}
	return result
}

func toVMPCIDevices(items []backend.AttachedPCIDevice) []vmstore.AttachedPCIDevice {
	result := make([]vmstore.AttachedPCIDevice, 0, len(items))
	for _, item := range items {
		result = append(result, vmstore.AttachedPCIDevice{ID: item.ID, PCI: item.PCI})
	}
	return result
}
