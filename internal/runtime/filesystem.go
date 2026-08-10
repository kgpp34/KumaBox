package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func (r *Runtime) AttachFilesystem(ctx context.Context, ref string, spec backend.FilesystemSpec) (*vmstore.VMRecord, error) {
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
	controller, ok := r.backend.(backend.FilesystemController)
	if !ok {
		return nil, fmt.Errorf("backend does not support virtio-fs")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	opID, err := r.beginOperation(ctx, operation.KindFilesystemAttach, rec.ID)
	if err != nil {
		return nil, err
	}
	attached, opErr := controller.AttachFilesystem(ctx, rec, spec)
	if opErr == nil {
		disks := append([]vmstore.AttachedFilesystem(nil), rec.AttachedFilesystems...)
		disks = append(disks, vmstore.AttachedFilesystem{ID: attached.ID, Tag: attached.Tag, Socket: attached.Socket})
		_, opErr = r.vmRecords.SetAttachedFilesystems(rec.ID, disks)
	}
	opErr = r.finishOperation(ctx, opID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}

func (r *Runtime) DetachFilesystem(ctx context.Context, ref, tag string) (*vmstore.VMRecord, error) {
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
	controller, ok := r.backend.(backend.FilesystemController)
	if !ok {
		return nil, fmt.Errorf("backend does not support virtio-fs")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	opID, err := r.beginOperation(ctx, operation.KindFilesystemDetach, rec.ID)
	if err != nil {
		return nil, err
	}
	opErr := controller.DetachFilesystem(ctx, rec, tag)
	if opErr == nil {
		kept := make([]vmstore.AttachedFilesystem, 0, len(rec.AttachedFilesystems))
		for _, fs := range rec.AttachedFilesystems {
			if fs.Tag != tag {
				kept = append(kept, fs)
			}
		}
		_, opErr = r.vmRecords.SetAttachedFilesystems(rec.ID, kept)
	}
	opErr = r.finishOperation(ctx, opID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}

func (r *Runtime) ListFilesystems(ctx context.Context, ref string) ([]backend.AttachedFilesystem, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	controller, ok := r.backend.(backend.FilesystemController)
	if !ok {
		return nil, fmt.Errorf("backend does not support virtio-fs")
	}
	return controller.ListFilesystems(ctx, rec)
}
