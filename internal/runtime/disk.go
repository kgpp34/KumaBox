package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func (r *Runtime) AttachDisk(ctx context.Context, ref string, spec backend.DiskSpec) (*vmstore.VMRecord, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(spec.Path) {
		return nil, fmt.Errorf("disk path must be absolute")
	}
	if _, err := os.Stat(spec.Path); err != nil {
		return nil, fmt.Errorf("stat disk: %w", err)
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for disk attach: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	controller, ok := r.backend.(backend.DiskController)
	if !ok {
		return nil, fmt.Errorf("backend does not support disk attach")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, operation.KindDiskAttach, rec.ID)
	if err != nil {
		return nil, err
	}
	attached, opErr := controller.AttachDisk(ctx, rec, spec)
	if opErr == nil {
		disks := append([]vmstore.AttachedDisk(nil), rec.AttachedDisks...)
		disks = append(disks, vmstore.AttachedDisk{ID: attached.ID, Name: attached.Name, Path: attached.Path, ReadOnly: attached.ReadOnly})
		_, opErr = r.vmRecords.SetAttachedDisks(rec.ID, disks)
	}
	opErr = r.finishOperation(ctx, operationID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}

func (r *Runtime) DetachDisk(ctx context.Context, ref, name string) (*vmstore.VMRecord, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for disk detach: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	controller, ok := r.backend.(backend.DiskController)
	if !ok {
		return nil, fmt.Errorf("backend does not support disk detach")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, operation.KindDiskDetach, rec.ID)
	if err != nil {
		return nil, err
	}
	opErr := controller.DetachDisk(ctx, rec, name)
	if opErr == nil {
		disks := make([]vmstore.AttachedDisk, 0, len(rec.AttachedDisks))
		for _, disk := range rec.AttachedDisks {
			if disk.Name != name {
				disks = append(disks, disk)
			}
		}
		_, opErr = r.vmRecords.SetAttachedDisks(rec.ID, disks)
	}
	opErr = r.finishOperation(ctx, operationID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}

func (r *Runtime) ListDisks(ctx context.Context, ref string) ([]backend.AttachedDisk, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	controller, ok := r.backend.(backend.DiskController)
	if !ok {
		return nil, fmt.Errorf("backend does not support disk list")
	}
	return controller.ListDisks(ctx, rec)
}
