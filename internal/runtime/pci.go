package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func (r *Runtime) AttachPCIDevice(ctx context.Context, ref string, spec backend.PCIDeviceSpec) (*vmstore.VMRecord, error) {
	return r.changePCIDevice(ctx, ref, spec, true)
}
func (r *Runtime) changePCIDevice(ctx context.Context, ref string, spec backend.PCIDeviceSpec, attach bool) (*vmstore.VMRecord, error) {
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
		return nil, err
	}
	defer lock.Release() //nolint:errcheck
	controller, ok := r.backend.(backend.PCIDeviceController)
	if !ok {
		return nil, fmt.Errorf("backend does not support VFIO PCI")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	kind := operation.KindPCIAttach
	if !attach {
		kind = operation.KindPCIDetach
	}
	opID, err := r.beginOperation(ctx, kind, rec.ID)
	if err != nil {
		return nil, err
	}
	var opErr error
	if attach {
		var device backend.AttachedPCIDevice
		device, opErr = controller.AttachPCIDevice(ctx, rec, spec)
		if opErr == nil {
			devices := append([]vmstore.AttachedPCIDevice(nil), rec.AttachedPCIDevices...)
			devices = append(devices, vmstore.AttachedPCIDevice{ID: device.ID, PCI: device.PCI})
			_, opErr = r.vmRecords.SetAttachedPCIDevices(rec.ID, devices)
		}
	} else {
		opErr = controller.DetachPCIDevice(ctx, rec, spec.ID)
		if opErr == nil {
			devices := make([]vmstore.AttachedPCIDevice, 0)
			for _, device := range rec.AttachedPCIDevices {
				if device.ID != spec.ID {
					devices = append(devices, device)
				}
			}
			_, opErr = r.vmRecords.SetAttachedPCIDevices(rec.ID, devices)
		}
	}
	opErr = r.finishOperation(ctx, opID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}
func (r *Runtime) DetachPCIDevice(ctx context.Context, ref, id string) (*vmstore.VMRecord, error) {
	return r.changePCIDevice(ctx, ref, backend.PCIDeviceSpec{ID: id}, false)
}
func (r *Runtime) ListPCIDevices(ctx context.Context, ref string) ([]backend.AttachedPCIDevice, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	controller, ok := r.backend.(backend.PCIDeviceController)
	if !ok {
		return nil, fmt.Errorf("backend does not support VFIO PCI")
	}
	return controller.ListPCIDevices(ctx, rec)
}
