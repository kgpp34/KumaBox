// Package resources composes KumaBox's persisted resource stores.
package resources

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/metastore"
	metasqlite "github.com/kumabox/kumabox/internal/metastore/sqlite"
	"github.com/kumabox/kumabox/internal/metering"
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
	Metering   *metering.Store
	Metadata   metastore.MetaEngine
	Guard      *lock.Guard
}

// NewStoreSetForConfig composes every persisted resource over the configured
// metadata backend. All SQLite-backed resources share one database and one
// transaction boundary; the JSON path keeps the existing file layout.
func NewStoreSetForConfig(cfg config.Config) (StoreSet, error) {
	if err := metasqlite.RefuseConversion(SQLiteMetadataPath(cfg)); err != nil {
		return StoreSet{}, err
	}
	if cfg.Metadata.Backend != "sqlite" {
		return NewStoreSet(cfg.Runtime.RootDir), nil
	}
	path := SQLiteMetadataPath(cfg)
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
		Metering:   metering.NewWithEngine(engine),
		Metadata:   engine,
		Guard:      lock.NewGuard(cfg.Runtime.RootDir),
	}, nil
}

// InitSQLiteMetadata creates the configured SQLite metadata database. Normal
// store construction deliberately refuses to create it implicitly.
func InitSQLiteMetadata(ctx context.Context, cfg config.Config) error {
	if cfg.Metadata.Backend != "sqlite" {
		return fmt.Errorf("metadata initialization requires the sqlite backend, got %q", cfg.Metadata.Backend)
	}
	return metasqlite.Init(ctx, SQLiteMetadataPath(cfg), sqliteDefinitions()...)
}

// SQLiteMetadataPath resolves the single database path used by all SQLite
// resource stores.
func SQLiteMetadataPath(cfg config.Config) string {
	if cfg.Metadata.Path != "" {
		return cfg.Metadata.Path
	}
	return filepath.Join(cfg.Runtime.RootDir, "metadata", "kumabox.db")
}

func sqliteDefinitions() []metasqlite.Namespace {
	return []metasqlite.Namespace{
		{Name: "vms", Tables: []metastore.Table{"vm-index"}},
		{Name: "images", Tables: []metastore.Table{"image-index"}},
		{Name: "snapshots", Tables: []metastore.Table{"snapshot-index"}},
		{Name: "networks", Tables: []metastore.Table{"network-index"}},
		{Name: "leases", Tables: []metastore.Table{"network-leases"}},
		{Name: "host-tap", Tables: []metastore.Table{"host-tap"}},
		{Name: "oci-content", Tables: []metastore.Table{"oci-content"}},
		{Name: "operations", Tables: []metastore.Table{"records"}},
		{Name: "references", Tables: []metastore.Table{"records"}},
		{Name: metering.Namespace, Tables: []metastore.Table{metering.Table}},
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
		Metering:   metering.New(rootDir),
		Guard:      lock.NewGuard(rootDir),
	}
}
