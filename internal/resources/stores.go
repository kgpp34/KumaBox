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
	"github.com/kumabox/kumabox/internal/reference"
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
	References state.ReferenceState
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
	engine, err := metasqlite.Open(path, sqliteDefinitions()...)
	if err != nil {
		return StoreSet{}, fmt.Errorf("open configured metadata backend: %w", err)
	}
	vm := vmstore.NewWithEngine(cfg.Runtime.RootDir, engine)
	return StoreSet{
		VM:         vm,
		Images:     imagestore.NewWithEngine(cfg.Runtime.RootDir, engine),
		Snapshots:  snapshot.NewStoreWithEngineAndVMReader(cfg.Runtime.RootDir, engine, vm),
		Networks:   kbnetwork.NewStoreWithEngines(cfg.Runtime.RootDir, engine, engine, engine),
		OCI:        ocistore.NewWithEngine(cfg.Runtime.RootDir, engine),
		Operations: operation.NewWithEngine(engine),
		References: reference.NewWithEngine(engine),
		Metadata:   engine,
	}, nil
}

func sqliteDefinitions() []metasqlite.Namespace {
	return []metasqlite.Namespace{
		{Name: "vms", Tables: []meta.Table{"vm-index"}},
		{Name: "images", Tables: []meta.Table{"image-index"}},
		{Name: "snapshots", Tables: []meta.Table{"snapshot-index"}},
		{Name: "networks", Tables: []meta.Table{"network-index"}},
		{Name: "leases", Tables: []meta.Table{"network-leases"}},
		{Name: "host-tap", Tables: []meta.Table{"host-tap"}},
		{Name: "oci-content", Tables: []meta.Table{"oci-content"}},
		{Name: "operations", Tables: []meta.Table{"records"}},
		{Name: "references", Tables: []meta.Table{"records"}},
	}
}

// NewStoreSet creates the default JSON-backed resource stores.
func NewStoreSet(rootDir string) StoreSet {
	vm := vmstore.New(rootDir)
	return StoreSet{
		VM:         vm,
		Images:     imagestore.New(rootDir),
		Snapshots:  snapshot.NewStoreWithVMReader(rootDir, vm),
		Networks:   kbnetwork.NewStore(rootDir),
		OCI:        ocistore.New(rootDir),
		Operations: operation.New(rootDir),
		References: reference.New(rootDir),
	}
}
