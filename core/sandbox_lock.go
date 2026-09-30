package core

import (
	"context"
	"errors"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
)

// sandboxLockRequest keeps lock diagnostics at the call site while sharing the
// two-resolve protocol required when a name can be reused concurrently.
type sandboxLockRequest struct {
	reference  string
	operation  string
	retryHint  string
	reportWait bool
}

// lockExistingSandbox first resolves only to select the stable ID lock. The
// second resolve, under that lock, supplies the authoritative state and
// generation for the operation. The caller must invoke unlock on every path.
func (s *SandboxService) lockExistingSandbox(ctx context.Context, request sandboxLockRequest) (record types.Sandbox, unlock func() error, err error) {
	record, err = s.dependencies.catalog.Resolve(ctx, request.reference)
	if err != nil {
		return types.Sandbox{}, nil, err
	}
	path, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, nil, err
	}
	if request.reportWait {
		if err := s.dependencies.reporter.Status("waiting for sandbox operation lock"); err != nil {
			return types.Sandbox{}, nil, err
		}
	}
	lock := filelock.New(path)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, nil, errdefs.Context(err, request.operation, request.reference, "lock", request.retryHint, false)
	}
	unlock = func() error { return lock.Unlock(context.WithoutCancel(ctx)) }
	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, nil, errors.Join(err, unlock())
	}
	return record, unlock, nil
}
