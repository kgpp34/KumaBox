package core

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/disk"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// applicationState owns adapters shared by snapshot and maintenance workflows.
// A single sandbox service opens the metadata engine and owns its lifetime.
type applicationState struct {
	configuration config.Config
	paths         snapshot.Paths
	sandboxPaths  sandbox.Paths
	sandboxes     sandboxCatalog
	snapshots     snapshotCatalog
	runtimes      *vmm.Registry
	store         metadata.Store
	lifecycle     *SandboxService
	images        imageGuard
	disks         disk.Backend
	networks      *network.Registry
	dnsServers    []string
	now           func() time.Time
	closeOnce     sync.Once
	closeErr      error
	// flushSnapshotAccess persists advisory usage after all request facades drain.
	flushSnapshotAccess func(context.Context) error
}

func (a *applicationState) close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.flushSnapshotAccess != nil {
			ctx, cancel := context.WithTimeout(context.Background(), a.configuration.Metadata.RetryLimit)
			a.closeErr = a.flushSnapshotAccess(ctx)
			cancel()
		}
		if a.lifecycle != nil {
			a.closeErr = errors.Join(a.closeErr, a.lifecycle.Close())
		} else if a.store != nil {
			a.closeErr = errors.Join(a.closeErr, a.store.Close())
		}
	})
	return a.closeErr
}

// Application groups the sandbox, snapshot, and maintenance services around
// one metadata engine. Close releases that engine once after all callers drain.
type Application struct {
	Images      *ImageStore
	Sandboxes   *SandboxService
	Snapshots   *SnapshotService
	Maintenance *MaintenanceService
	state       *applicationState
}

// OpenApplication assembles cross-module workflows without reopening the store.
func OpenApplication(ctx context.Context, configuration config.Config, reporter SnapshotReporter) (*Application, error) {
	lifecycle, err := OpenSandbox(ctx, configuration, nil)
	if err != nil {
		return nil, err
	}
	paths, err := snapshot.NewPaths(configuration.Paths)
	if err != nil {
		return nil, errors.Join(err, lifecycle.Close())
	}
	if err := paths.Ensure(); err != nil {
		return nil, errors.Join(err, lifecycle.Close())
	}
	if reporter == nil {
		reporter = discardSnapshotReporter{}
	}
	snapshotStore := snapshotcatalog.New(lifecycle.dependencies.store)
	if configuration.Snapshots.AccessFlushInterval > 0 {
		snapshotStore, err = snapshotcatalog.NewBuffered(lifecycle.dependencies.store, configuration.Snapshots.AccessBufferSize)
		if err != nil {
			return nil, errors.Join(err, lifecycle.Close())
		}
	}
	state := &applicationState{
		configuration: configuration, paths: paths,
		sandboxPaths: lifecycle.dependencies.paths, sandboxes: lifecycle.dependencies.catalog,
		snapshots: snapshotStore, runtimes: lifecycle.dependencies.runtimes,
		store: lifecycle.dependencies.store, lifecycle: lifecycle, images: lifecycle.dependencies.images,
		disks: lifecycle.dependencies.disks, networks: lifecycle.dependencies.networks,
		dnsServers: lifecycle.dependencies.dnsServers, now: time.Now,
		flushSnapshotAccess: snapshotStore.FlushAccesses,
	}
	snapshots := &SnapshotService{applicationState: state, reporter: reporter, newID: types.NewSnapshotID}
	return &Application{
		Images:      &ImageStore{Paths: lifecycle.dependencies.imagePaths, Catalog: imagecatalog.New(lifecycle.dependencies.store, imagecatalog.WithImageUsage(imageUsage{})), options: configuration.Images, store: lifecycle.dependencies.store},
		Sandboxes:   lifecycle,
		Snapshots:   snapshots,
		Maintenance: &MaintenanceService{applicationState: state, snapshotService: snapshots},
		state:       state,
	}, nil
}

// Close releases shared resources once, including when service facades close.
func (a *Application) Close() error {
	if a == nil {
		return nil
	}
	return a.state.close()
}

// FlushSnapshotAccess persists advisory usage in one batch. It never changes
// lifecycle durability; daemon owns periodic calls and Close performs a final flush.
func (a *Application) FlushSnapshotAccess(ctx context.Context) error {
	return a.state.flushSnapshotAccess(ctx)
}

// WithReporter returns a request-local view sharing adapters without changing
// another request's reporter. Only Application.Close owns the shared store.
func (s *SandboxService) WithReporter(reporter SandboxReporter) *SandboxService {
	copy := *s
	if reporter == nil {
		reporter = discardReporter{}
	}
	copy.dependencies.reporter = reporter
	return &copy
}

// WithReporter isolates snapshot progress while preserving the common leases.
func (s *SnapshotService) WithReporter(reporter SnapshotReporter) *SnapshotService {
	copy := *s
	if reporter == nil {
		reporter = discardSnapshotReporter{}
	}
	copy.reporter = reporter
	return &copy
}
