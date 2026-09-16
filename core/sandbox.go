package core

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/kumabox/kumabox/disk"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/sandbox"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const cleanupTimeout = 10 * time.Second

// CreateSandboxRequest contains user intent before image aliases are resolved.
type CreateSandboxRequest struct {
	// ImageReference is an existing local image alias or manifest digest.
	ImageReference string
	// Config contains the immutable name and guest resource shape.
	Config types.SandboxConfig
}

// imageGuard is the image capability consumed by sandbox creation.
type imageGuard interface {
	WithAvailable(context.Context, string, func(types.Image) error) (types.Image, error)
}

// sandboxCreator is the metadata capability consumed by sandbox creation.
type sandboxCreator interface {
	Reserve(context.Context, string, types.Digest, types.Sandbox) error
	MarkCreated(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkError(context.Context, types.SandboxID, uint64, types.SandboxFailure, time.Time) (types.Sandbox, error)
	Forget(context.Context, types.SandboxID, uint64) error
}

// sandboxReader is the metadata capability consumed by sandbox queries and lookup.
type sandboxReader interface {
	Resolve(context.Context, string) (types.Sandbox, error)
	List(context.Context) ([]types.Sandbox, error)
}

// sandboxRemover is the metadata capability consumed by sandbox removal.
type sandboxRemover interface {
	BeginDelete(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	FinalizeDelete(context.Context, types.SandboxID, uint64) error
}

// cowStore is the private writable-disk capability consumed by sandbox creation.
type cowStore interface {
	Prepare(context.Context, types.SandboxID, int64) error
	Remove(context.Context, types.SandboxID) error
}

// SandboxReporter receives user-visible stages without controlling workflows.
type SandboxReporter interface {
	Status(string) error
	Committed(types.Sandbox) error
}

// SandboxService owns application ordering and resources for sandbox commands.
type SandboxService struct {
	// paths supplies the stable per-sandbox operation lock path.
	paths sandbox.Paths
	// images closes the verify/pin race with image removal.
	images imageGuard
	// creator commits identity, image references, and create transitions.
	creator sandboxCreator
	// reader supplies consistent sandbox snapshots without changing state.
	reader sandboxReader
	// remover commits generation-fenced delete transitions.
	remover sandboxRemover
	// cows prepares and cleans the sandbox-owned writable disk.
	cows cowStore
	// reporter emits progress independently of command results.
	reporter SandboxReporter
	// newID and now are replaceable in same-package tests.
	newID func() (types.SandboxID, error)
	now   func() time.Time
	// store is the shared metadata engine closed after the command finishes.
	store metadata.Store
}

// newSandboxService connects the explicit capabilities needed by sandbox commands.
func newSandboxService(paths sandbox.Paths, images imageGuard, creator sandboxCreator, reader sandboxReader, remover sandboxRemover, cows cowStore, reporter SandboxReporter) *SandboxService {
	if reporter == nil {
		reporter = discardReporter{}
	}
	return &SandboxService{paths: paths, images: images, creator: creator, reader: reader, remover: remover, cows: cows, reporter: reporter, newID: types.NewSandboxID, now: time.Now}
}

// OpenSandbox assembles the image guard, metadata catalog, and ext4 COW adapter
// used by sandbox commands. The caller must close the returned service.
//
//	shared SQLite -> image catalog <---- transaction reader ---- sandbox catalog
//	       |              ^                                      |
//	       +---- usage ---+---- image guard + ext4 COW ----------> service
func OpenSandbox(ctx context.Context, roots storage.Roots, reporter SandboxReporter) (*SandboxService, error) {
	imagePaths, err := images.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	sandboxPaths, err := sandbox.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(imagePaths.Ensure(), sandboxPaths.Ensure()); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, imagePaths.MetadataDB(), metadataCollections(), sqlite.DefaultOptions())
	if err != nil {
		return nil, err
	}
	imageCatalog := imagecatalog.New(store, imagecatalog.WithImageUsage(sandboxcatalog.Usage{}))
	sandboxCatalog := sandboxcatalog.New(store, imagecatalog.Reader{})
	service := newSandboxService(sandboxPaths, images.NewGuard(imagePaths, imageCatalog), sandboxCatalog, sandboxCatalog, sandboxCatalog, disk.NewExt4(sandboxPaths), reporter)
	service.store = store
	return service, nil
}

// Close releases the shared metadata engine owned by the service.
func (s *SandboxService) Close() error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.Close()
}

// Create reserves identity and image usage before preparing the private disk.
// Only the final generation-fenced transition makes the disk startable.
//
//	validate -> ID lock -> image locks + reservation -> sparse ext4 COW -> Created
//	                            |                         |
//	                            +---- failure cleanup <---+
func (s *SandboxService) Create(ctx context.Context, request CreateSandboxRequest) (result types.Sandbox, returnErr error) {
	if s == nil || s.images == nil || s.creator == nil || s.cows == nil || s.reporter == nil || s.newID == nil || s.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if request.ImageReference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("IMAGE must not be empty"))
	}
	if err := request.Config.Validate(); err != nil {
		return types.Sandbox{}, err
	}
	if int(request.Config.CPUs) > runtime.NumCPU() { //nolint:gosec // Config validation bounds CPUs to a small positive value
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("requested %d vCPUs exceeds available host CPUs (%d)", request.Config.CPUs, runtime.NumCPU()))
	}
	if err := s.reporter.Status("resolving and checking image"); err != nil {
		return types.Sandbox{}, err
	}
	id, err := s.newID()
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(id)
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

	createdAt := s.now().UTC()
	record := types.Sandbox{}
	reserved := false
	_, err = s.images.WithAvailable(ctx, request.ImageReference, func(image types.Image) error {
		record = types.Sandbox{
			ID: id, Config: request.Config, ImageDigest: image.ManifestDigest,
			State: types.SandboxStateCreating, Generation: 1,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := s.creator.Reserve(ctx, request.ImageReference, image.ManifestDigest, record); err != nil {
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
	if err := s.reporter.Status("creating sparse ext4 disk"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	if err := s.cows.Prepare(ctx, id, request.Config.Storage); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "disk", err)
	}
	if err := s.reporter.Status("committing created state"); err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "report", err)
	}
	created, err := s.creator.MarkCreated(ctx, id, record.Generation, s.now().UTC())
	if err != nil {
		return types.Sandbox{}, s.compensate(ctx, record, "commit", err)
	}
	result = created
	if err := s.reporter.Committed(created); err != nil {
		return created, errdefs.Context(err, "create sandbox", request.Config.Name, "report", "sandbox was created; inspect it before retrying", true)
	}
	return created, nil
}

// List returns a consistent sandbox snapshot. Unless includeAll is true, only
// states associated with an active VMM operation are returned.
func (s *SandboxService) List(ctx context.Context, includeAll bool) ([]types.Sandbox, error) {
	if s == nil || s.reader == nil {
		return nil, errors.New("sandbox service is not configured")
	}
	records, err := s.reader.List(ctx)
	if err != nil {
		return nil, err
	}
	if includeAll {
		return records, nil
	}
	active := make([]types.Sandbox, 0, len(records))
	for _, record := range records {
		switch record.State {
		case types.SandboxStateStarting, types.SandboxStateRunning, types.SandboxStateStopping:
			active = append(active, record)
		}
	}
	return active, nil
}

// Remove records cleanup intent before deleting the COW directory and releases
// the name and image reference only after filesystem cleanup succeeds.
//
//	resolve -> sandbox lock -> Deleting -> remove files -> forget record + name
//	                              |                            |
//	                              +---- retry resumes here <---+
func (s *SandboxService) Remove(ctx context.Context, reference string) (result types.Sandbox, returnErr error) {
	if s == nil || s.remover == nil || s.cows == nil || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	if err := s.reporter.Status("resolving sandbox"); err != nil {
		return types.Sandbox{}, err
	}
	record, err := s.reader.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("waiting for sandbox operation lock"); err != nil {
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
	if err := s.reporter.Status("marking sandbox for deletion"); err != nil {
		return types.Sandbox{}, err
	}
	deleting, err := s.remover.BeginDelete(ctx, record.ID, record.Generation, s.now().UTC())
	if err != nil {
		return types.Sandbox{}, err
	}
	committed = true
	result = deleting
	if err := s.reporter.Status("removing sandbox disk"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.cows.Remove(ctx, deleting.ID); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "disk cleanup", "retry removal to finish cleanup", true)
	}
	if err := s.reporter.Status("releasing metadata and image reference"); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "retry removal to finish cleanup", true)
	}
	if err := s.remover.FinalizeDelete(ctx, deleting.ID, deleting.Generation); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "finalize", "retry removal to finish cleanup", true)
	}
	if err := s.reporter.Committed(deleting); err != nil {
		return deleting, errdefs.Context(err, "remove sandbox", reference, "report", "sandbox was deleted; do not retry", true)
	}
	return deleting, nil
}

// compensate removes the owned disk before forgetting the Creating reservation.
// If cleanup cannot be proven complete, Error retains the resource owner and image pin.
func (s *SandboxService) compensate(ctx context.Context, record types.Sandbox, phase string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	removeErr := s.cows.Remove(cleanupCtx, record.ID)
	if removeErr == nil {
		forgetErr := s.creator.Forget(cleanupCtx, record.ID, record.Generation)
		if forgetErr == nil {
			return errdefs.Context(cause, "create sandbox", record.Config.Name, phase, "fix the failure and retry", false)
		}
		removeErr = forgetErr
	}
	failure := types.SandboxFailure{Phase: phase, Message: errors.Join(cause, removeErr).Error()}
	_, markErr := s.creator.MarkError(cleanupCtx, record.ID, record.Generation, failure, s.now().UTC())
	return errdefs.Context(errors.Join(cause, removeErr, markErr), "create sandbox", record.Config.Name, phase, "inspect or remove the retained error sandbox", false)
}

// discardReporter keeps reporting optional for non-CLI consumers.
type discardReporter struct{}

func (discardReporter) Status(string) error           { return nil }
func (discardReporter) Committed(types.Sandbox) error { return nil }
