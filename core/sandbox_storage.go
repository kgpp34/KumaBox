package core

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
)

// Create reserves identity and image usage before preparing the private disk.
// Only the final generation-fenced transition makes the disk startable.
//
//	validate -> ID lock -> image locks + reservation -> sparse ext4 COW -> Created
//	                            |                         |
//	                            +---- failure cleanup <---+
func (s *SandboxService) Create(ctx context.Context, request CreateSandboxRequest) (result types.Sandbox, returnErr error) {
	if s == nil || s.dependencies.images == nil || s.dependencies.catalog == nil || s.dependencies.disks == nil || s.dependencies.runtimes.Len() == 0 || s.dependencies.reporter == nil || s.dependencies.newID == nil || s.dependencies.now == nil || s.dependencies.cleanupTimeout <= 0 {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if request.ImageReference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("IMAGE must not be empty"))
	}
	if err := request.Config.Validate(); err != nil {
		return types.Sandbox{}, err
	}
	if request.VMM == "" {
		request.VMM = s.dependencies.defaultVMM
	}
	if _, err := s.dependencies.runtimes.Backend(request.VMM); err != nil {
		return types.Sandbox{}, err
	}
	if int(request.Config.CPUs) > runtime.NumCPU() { //nolint:gosec // Config validation bounds CPUs to a small positive value
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("requested %d vCPUs exceeds available host CPUs (%d)", request.Config.CPUs, runtime.NumCPU()))
	}
	if err := s.dependencies.reporter.Status("resolving and checking image"); err != nil {
		return types.Sandbox{}, err
	}
	id, err := s.dependencies.newID()
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(id)
	if err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "create sandbox", request.Config.Name, "lock", "retry the create", false)
	}
	defer func() {
		unlockErr := lock.Unlock(context.WithoutCancel(ctx))
		if unlockErr != nil {
			committed := result.State == types.SandboxStateCreated
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "create sandbox", request.Config.Name, "unlock", "inspect the sandbox before retrying", committed)
		}
	}()

	createdAt := s.dependencies.now().UTC()
	record := types.Sandbox{}
	reserved := false
	_, err = s.dependencies.images.WithAvailable(ctx, request.ImageReference, func(image types.Image) error {
		record = types.Sandbox{
			ID: id, Config: request.Config, ImageDigest: image.ManifestDigest,
			VMM:   request.VMM,
			State: types.SandboxStateCreating, Generation: 1,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := s.dependencies.catalog.Reserve(ctx, request.ImageReference, image.ManifestDigest, record); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		if reserved {
			return types.Sandbox{}, s.compensate(ctx, record, "image unlock", err)
		}
		return types.Sandbox{}, errdefs.Context(err, "create sandbox", request.Config.Name, "reserve", "check the image and sandbox name", false)
	}
	if err := s.dependencies.reporter.Status("creating sparse ext4 disk"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	if err := s.dependencies.disks.Prepare(ctx, id, request.Config.Storage); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "disk", err)
	}
	if err := s.dependencies.reporter.Status("committing created state"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	created, err := s.dependencies.catalog.MarkCreated(ctx, id, record.Generation, s.dependencies.now().UTC())
	if err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "commit", err)
	}
	result = created
	if err := s.dependencies.reporter.Committed(created); err != nil {
		return created, errdefs.Context(err, "create sandbox", request.Config.Name, "report", "sandbox was created; inspect it before retrying", true)
	}
	return created, nil
}

// Remove records cleanup intent before deleting the COW directory and releases
// the name and image reference only after filesystem cleanup succeeds.
//
//	resolve -> sandbox lock -> Deleting -> remove files -> forget record + name
//	                              |                            |
//	                              +---- retry resumes here <---+
func (s *SandboxService) Remove(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.disks == nil || s.dependencies.reporter == nil || s.dependencies.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.dependencies.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.dependencies.reporter.Status("waiting for sandbox operation lock"); err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "remove sandbox", reference, "lock", "retry the removal", false)
	}
	committed := false
	defer func() {
		unlockErr := lock.Unlock(context.WithoutCancel(ctx))
		if unlockErr != nil {
			returnErr = errdefs.Context(errors.Join(returnErr, unlockErr), "remove sandbox", reference, "unlock", "inspect the sandbox removal state before retrying", committed)
		}
	}()
	if err := s.dependencies.reporter.Status("marking sandbox for deletion"); err != nil {
		return types.Sandbox{}, err
	}
	deleting, err := s.dependencies.catalog.BeginDelete(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
	if err != nil {
		return types.Sandbox{}, err
	}
	committed = true
	result = deleting
	if err := s.dependencies.reporter.Status("removing sandbox disk"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.dependencies.disks.Remove(ctx, deleting.ID); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "disk cleanup", "retry removal to finish cleanup", true)
	}
	if err := s.dependencies.reporter.Status("releasing metadata and image reference"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.dependencies.catalog.FinalizeDelete(ctx, deleting.ID, deleting.Generation); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "finalize", "retry removal to finish cleanup", true)
	}
	if err := s.dependencies.reporter.Committed(deleting); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "sandbox was deleted; do not retry", true)
	}
	return deleting, nil
}

// compensate removes the owned disk before forgetting the Creating reservation.
// If cleanup cannot be proven complete, Error retains the resource owner and image pin.
func (s *SandboxService) compensate(ctx context.Context, record types.Sandbox, phase string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.dependencies.cleanupTimeout)
	defer cancel()
	removeErr := s.dependencies.disks.Remove(cleanupCtx, record.ID)
	if removeErr == nil {
		forgetErr := s.dependencies.catalog.Forget(cleanupCtx, record.ID, record.Generation)
		if forgetErr == nil {
			return errdefs.Context(cause, "create sandbox", record.Config.Name, phase, "fix the failure and retry", false)
		}
		removeErr = forgetErr
	}
	failure := types.SandboxFailure{Phase: phase, Message: errors.Join(cause, removeErr).Error()}
	_, markErr := s.dependencies.catalog.MarkError(cleanupCtx, record.ID, record.Generation, failure, s.dependencies.now().UTC())
	return errdefs.Context(errors.Join(cause, removeErr, markErr), "create sandbox", record.Config.Name, phase, "inspect or remove the retained error sandbox", false)
}
