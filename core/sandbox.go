package core

import (
	"context"
	"errors"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/disk"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/network/cni"
	"github.com/kumabox/kumabox/sandbox"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// CreateSandboxRequest contains user intent before image aliases are resolved.
type CreateSandboxRequest struct {
	// ImageReference is an existing local image alias or manifest digest.
	ImageReference string
	// Config contains the immutable name and guest resource shape.
	Config types.SandboxConfig
	// VMM selects the runtime backend; empty uses the configured default.
	VMM types.VMMType
}

// imageGuard is the image capability consumed by sandbox creation.
type imageGuard interface {
	WithAvailable(context.Context, string, func(types.Image) error) (types.Image, error)
}

// sandboxCatalog is the complete metadata capability consumed by the sandbox
// application service. One adapter owns the aggregate and its state machine,
// so the service receives it once instead of under several role aliases.
type sandboxCatalog interface {
	Reserve(context.Context, string, types.Digest, types.Sandbox) error
	MarkCreated(context.Context, types.SandboxID, uint64, types.NetworkSetup, time.Time) (types.Sandbox, error)
	MarkError(context.Context, types.SandboxID, uint64, types.SandboxFailure, time.Time) (types.Sandbox, error)
	Forget(context.Context, types.SandboxID, uint64) error
	Resolve(context.Context, string) (types.Sandbox, error)
	List(context.Context) ([]types.Sandbox, error)
	BeginDelete(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	FinalizeDelete(context.Context, types.SandboxID, uint64) error
	BeginStart(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkRunning(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkStartError(context.Context, types.SandboxID, uint64, types.SandboxFailure, time.Time) (types.Sandbox, error)
	BeginStop(context.Context, types.SandboxID, uint64, time.Time) (types.Sandbox, error)
	MarkStopped(context.Context, types.SandboxID, uint64, types.SandboxState, time.Time) (types.Sandbox, error)
}

var (
	_ imageGuard     = (*images.Guard)(nil)
	_ sandboxCatalog = (*sandboxcatalog.Store)(nil)
)

// SandboxReporter receives user-visible stages without controlling workflows.
type SandboxReporter interface {
	Status(string) error
	Committed(types.Sandbox) error
}

// sandboxDependencies names every adapter and policy consumed by SandboxService.
// Keeping construction package-local avoids turning test seams into public API.
type sandboxDependencies struct {
	// paths supplies the stable per-sandbox operation lock path.
	paths sandbox.Paths
	// images closes the verify/pin race with image removal.
	images imageGuard
	// catalog owns sandbox records, names, image pins, and state transitions.
	catalog sandboxCatalog
	// disks prepares and cleans sandbox-owned writable disks.
	disks disk.Backend
	// networks routes persisted network identities to provider adapters.
	networks *network.Registry
	// defaultNetwork selects the provider for newly created networked sandboxes.
	defaultNetwork types.NetworkBackend
	// imagePaths derives immutable artifacts after the image guard verifies them.
	imagePaths images.Paths
	// runtimes route persisted VMM identities to process adapters.
	runtimes *vmm.Registry
	// defaultVMM selects the runtime when create does not specify one.
	defaultVMM types.VMMType
	// cleanupTimeout bounds compensation that outlives caller cancellation.
	cleanupTimeout time.Duration
	// dnsServers are rendered into static guest boot network parameters.
	dnsServers []string
	// reporter emits progress independently of command results.
	reporter SandboxReporter
	// newID and now are replaceable in same-package tests.
	newID func() (types.SandboxID, error)
	now   func() time.Time
	// store is the shared metadata engine closed after the command finishes.
	store metadata.Store
}

// SandboxService owns application ordering and resources for sandbox commands.
type SandboxService struct {
	dependencies sandboxDependencies
}

// newSandboxService validates and records the explicit capabilities needed by
// sandbox commands. Defaults are limited to deterministic process-local seams.
func newSandboxService(dependencies sandboxDependencies) (*SandboxService, error) {
	if dependencies.images == nil || dependencies.catalog == nil || dependencies.disks == nil || dependencies.networks == nil || dependencies.runtimes.Len() == 0 {
		return nil, errors.New("sandbox service adapters are incomplete")
	}
	if dependencies.cleanupTimeout <= 0 {
		return nil, errors.New("sandbox cleanup timeout must be positive")
	}
	if _, err := dependencies.runtimes.Backend(dependencies.defaultVMM); err != nil {
		return nil, err
	}
	if _, err := dependencies.networks.Provider(dependencies.defaultNetwork); err != nil {
		return nil, err
	}
	if dependencies.reporter == nil {
		dependencies.reporter = discardReporter{}
	}
	if dependencies.newID == nil {
		dependencies.newID = types.NewSandboxID
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	return &SandboxService{dependencies: dependencies}, nil
}

// OpenSandbox assembles the image guard, metadata catalog, and ext4 COW adapter
// used by sandbox commands. The caller must close the returned service.
//
//	shared SQLite -> image catalog <---- transaction reader ---- sandbox catalog
//	       |              ^                                      |
//	       +---- usage ---+---- image guard + ext4 COW ----------> service
func OpenSandbox(ctx context.Context, configuration config.Config, reporter SandboxReporter) (*SandboxService, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	dnsServers, err := configuration.Network.DNSServers()
	if err != nil {
		return nil, err
	}
	imagePaths, err := images.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	sandboxPaths, err := sandbox.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	runtimes, err := openVMMRegistry(configuration)
	if err != nil {
		return nil, err
	}
	defaultVMM := configuration.VMM.Default
	if _, err := runtimes.Backend(defaultVMM); err != nil {
		return nil, err
	}
	disks, err := disk.NewExt4(sandboxPaths, configuration.Sandbox.Ext4Binary)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(imagePaths.Ensure(), sandboxPaths.Ensure()); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, imagePaths.MetadataDB(), metadataCollections(), sqlite.Options{
		BusyTimeout: configuration.Metadata.BusyTimeout,
		RetryLimit:  configuration.Metadata.RetryLimit,
	})
	if err != nil {
		return nil, err
	}
	cacheDir, err := storage.Join(configuration.Paths.Data, "cni", "cache")
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	cniProvider, err := cni.New(cni.Options{
		ConfDir:         configuration.Network.CNI.ConfDir,
		BinDir:          configuration.Network.CNI.BinDir,
		CacheDir:        cacheDir,
		NamespacePrefix: configuration.Network.NamespacePrefix(),
		CleanupTimeout:  configuration.Network.CleanupTimeout,
	}, store)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	networks, err := network.NewRegistry(cniProvider)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	imageCatalog := imagecatalog.New(store, imagecatalog.WithImageUsage(sandboxcatalog.Usage{}))
	sandboxCatalog := sandboxcatalog.New(store, imagecatalog.Reader{})
	service, err := newSandboxService(sandboxDependencies{
		paths: sandboxPaths, imagePaths: imagePaths, images: images.NewGuard(imagePaths, imageCatalog),
		catalog: sandboxCatalog, disks: disks, networks: networks, runtimes: runtimes, reporter: reporter,
		store: store, defaultVMM: defaultVMM, defaultNetwork: types.NetworkBackendCNI,
		cleanupTimeout: max(configuration.Sandbox.CleanupTimeout, configuration.Network.CleanupTimeout),
		dnsServers:     dnsServers,
	})
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return service, nil
}

// Close releases the shared metadata engine owned by the service.
func (s *SandboxService) Close() error {
	if s == nil || s.dependencies.store == nil {
		return nil
	}
	return s.dependencies.store.Close()
}

// networkProvider resolves the provider that owns a sandbox's durable network
// state. Creating or retained-error records without a published setup fall
// back to the configured creation backend so cleanup can still resume.
func (s *SandboxService) networkProvider(record types.Sandbox) (network.Provider, bool, error) {
	if record.Config.NICs == 0 && record.Network.Backend == "" {
		return nil, false, nil
	}
	backend := record.Network.Backend
	if backend == "" {
		backend = s.dependencies.defaultNetwork
	}
	provider, err := s.dependencies.networks.Provider(backend)
	return provider, true, err
}

// Run creates and starts one sandbox as a single application use case. Create
// owns compensation until Created is durable; after that point a failed start
// retains the sandbox and its failure state for inspection and retry.
//
//	image + config -> Create -> Created -> Start -> Running
//	                              |          |
//	                              +----------+-> retained on start failure
func (s *SandboxService) Run(ctx context.Context, request CreateSandboxRequest) (types.Sandbox, error) {
	created, err := s.Create(ctx, request)
	if err != nil {
		return types.Sandbox{}, err
	}
	running, err := s.Start(ctx, created.ID.String())
	if err != nil {
		return created, errdefs.Context(
			err, "run sandbox", request.Config.Name, "start",
			"inspect the retained sandbox and VMM log before retrying", true,
		)
	}
	return running, nil
}

// List returns a consistent sandbox snapshot. Unless includeAll is true, only
// states associated with an active VMM operation are returned.
func (s *SandboxService) List(ctx context.Context, includeAll bool) ([]types.Sandbox, error) {
	if s == nil || s.dependencies.catalog == nil {
		return nil, errors.New("sandbox service is not configured")
	}
	records, err := s.dependencies.catalog.List(ctx)
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

// Inspect resolves one sandbox snapshot without changing persistent or runtime state.
// Runtime observation will be added here when the VMM lifecycle is available.
func (s *SandboxService) Inspect(ctx context.Context, reference string) (types.Sandbox, error) {
	if s == nil || s.dependencies.catalog == nil {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SANDBOX must not be empty"))
	}
	return s.dependencies.catalog.Resolve(ctx, reference)
}

// discardReporter keeps reporting optional for non-CLI consumers.
type discardReporter struct{}

func (discardReporter) Status(string) error           { return nil }
func (discardReporter) Committed(types.Sandbox) error { return nil }
