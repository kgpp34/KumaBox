package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/snapshot"
)

// VerifyNativeSnapshot performs a read-only restore preflight against an
// existing VM. It shares the same compatibility path used by restore.
func (r *Runtime) VerifyNativeSnapshot(ctx context.Context, snapshotRef, vmRef string) (*snapshot.Manifest, error) {
	rec, err := r.vmStore.Inspect(vmRef)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for snapshot verification: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.vmStore.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	inspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}
	host, err := inspector.InspectNativeHost(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	return r.storeSet.Snapshots.VerifyNative(ctx, snapshotRef, snapshot.NativeVerifyTarget{VM: rec, Host: host})
}
