package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

// ResizeBalloon serializes runtime memory reclaim with capture, stop and removal.
// The durable resource shape and create-time target remain independent of reclaim.
func (s *SandboxService) ResizeBalloon(
	ctx context.Context,
	reference string,
	target types.BalloonTarget,
) (result types.BalloonStatus, returnErr error) {
	if err := target.Validate(); err != nil {
		return result, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	record, unlock, err := s.lockExistingSandbox(ctx, sandboxLockRequest{
		reference: reference, operation: "resize sandbox balloon", retryHint: "retry balloon resize",
	})
	if err != nil {
		return result, err
	}
	defer func() {
		returnErr = errdefs.WithContext(errors.Join(returnErr, unlock()), errdefs.ContextInfo{
			Operation: "resize sandbox balloon", Entity: reference,
			Phase: "resize balloon", Action: "inspect the live balloon before retrying",
		})
	}()
	if record.State != types.SandboxStateRunning || record.Generation < 2 {
		return result, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not running", record.ID, record.State))
	}
	if _, err := target.Resolve(record.Config.Memory); err != nil {
		return result, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return result, err
	}
	controller, ok := backend.(vmm.BalloonResizer)
	if !ok {
		return result, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("VMM does not support balloon resize"))
	}
	process, found, err := backend.Locate(ctx, record.ID, record.Generation-1)
	if err != nil {
		return result, err
	}
	if !found {
		return result, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("sandbox VMM is absent"))
	}
	return controller.ResizeBalloon(ctx, process, target)
}
