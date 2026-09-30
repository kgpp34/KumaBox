package core

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/disk"
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
}

func (a *applicationState) close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.lifecycle != nil {
			a.closeErr = a.lifecycle.Close()
		} else if a.store != nil {
			a.closeErr = a.store.Close()
		}
	})
	return a.closeErr
}

// Application groups the sandbox, snapshot, and maintenance services around
// one metadata engine. Close releases that engine once after command work.
type Application struct {
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
	state := &applicationState{
		configuration: configuration, paths: paths,
		sandboxPaths: lifecycle.dependencies.paths, sandboxes: lifecycle.dependencies.catalog,
		snapshots: snapshotcatalog.New(lifecycle.dependencies.store), runtimes: lifecycle.dependencies.runtimes,
		store: lifecycle.dependencies.store, lifecycle: lifecycle, images: lifecycle.dependencies.images,
		disks: lifecycle.dependencies.disks, networks: lifecycle.dependencies.networks,
		dnsServers: lifecycle.dependencies.dnsServers, now: time.Now,
	}
	snapshots := &SnapshotService{applicationState: state, reporter: reporter, newID: types.NewSnapshotID}
	return &Application{
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
