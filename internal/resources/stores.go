// Package resources composes KumaBox's persisted resource stores.
package resources

import (
	"fmt"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/meta"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// StoreSet is the complete persisted resource composition used by one
// KumaBox process. Store implementations can be replaced before handing the
// set to Runtime, GC, or a CLI command.
type StoreSet struct {
	VM         state.VMState
	Images     state.ImageState
	Snapshots  state.SnapshotState
	Networks   state.NetworkState
	OCI        state.OCIState
	Operations state.OperationState
	Metadata   meta.MetaEngine
}

// NewStoreSetForConfig composes every persisted resource over the configured
// metadata backend. All SQLite-backed resources share one database and one
// transaction boundary; the JSON path keeps the existing file layout.
func NewStoreSetForConfig(cfg config.Config) (StoreSet, error) {
	if cfg.Metadata.Backend != "sqlite" {
		return NewStoreSet(cfg.Runtime.RootDir), nil
	}
	path := cfg.Metadata.Path
	if path == "" {
		path = filepath.Join(cfg.Runtime.RootDir, "metadata", "kumabox.db")
	}
	engine, err := metasqlite.Open(path,
		metasqlite.Namespace{Name: "vms", Tables: []meta.Table{"vm-index"}},
		metasqlite.Namespace{Name: "images", Tables: []meta.Table{"image-index"}},
		metasqlite.Namespace{Name: "snapshots", Tables: []meta.Table{"snapshot-index"}},
		metasqlite.Namespace{Name: "networks", Tables: []meta.Table{"network-index"}},
		metasqlite.Namespace{Name: "leases", Tables: []meta.Table{"network-leases"}},
		metasqlite.Namespace{Name: "host-tap", Tables: []meta.Table{"host-tap"}},
		metasqlite.Namespace{Name: "oci-content", Tables: []meta.Table{"oci-content"}},
		metasqlite.Namespace{Name: "operations", Tables: []meta.Table{"records"}},
	)
	if err != nil {
		return StoreSet{}, fmt.Errorf("open configured metadata backend: %w", err)
	}
	return StoreSet{
		VM:         vmstore.NewWithEngine(cfg.Runtime.RootDir, engine),
		Images:     imagestore.NewWithEngine(cfg.Runtime.RootDir, engine),
		Snapshots:  snapshot.NewStoreWithEngine(cfg.Runtime.RootDir, engine),
		Networks:   kbnetwork.NewStoreWithEngines(cfg.Runtime.RootDir, engine, engine, engine),
		OCI:        ocistore.NewWithEngine(cfg.Runtime.RootDir, engine),
		Operations: operation.NewWithEngine(engine),
		Metadata:   engine,
	}, nil
}

// NewStoreSet creates the default JSON-backed resource stores.
func NewStoreSet(rootDir string) StoreSet {
	return StoreSet{
		VM:         vmstore.New(rootDir),
		Images:     imagestore.New(rootDir),
		Snapshots:  snapshot.NewStore(rootDir),
		Networks:   kbnetwork.NewStore(rootDir),
		OCI:        ocistore.New(rootDir),
		Operations: operation.New(rootDir),
	}
}
