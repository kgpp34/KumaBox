// Package state defines and opens KumaBox's durable state capabilities.
package state

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/image/oci"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/meta"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	"github.com/kumabox/kumabox/internal/metering"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vm"
)

// Set is the complete persisted resource composition used by one
// KumaBox process. Store implementations can be replaced before handing the
// set to Runtime, GC, or a CLI command.
type Set struct {
	VM         VMState
	Images     ImageState
	Snapshots  SnapshotState
	Networks   NetworkState
	OCI        OCIState
	Operations OperationState
	References ReferenceState
	Metering   *metering.Store
	Metadata   meta.MetaEngine
	Guard      *lock.Guard
}

// Open composes every persisted resource over the configured
// metadata backend. All SQLite-backed resources share one database and one
// transaction boundary; the JSON path keeps the existing file layout.
func Open(cfg config.Config) (Set, error) {
	if err := metasqlite.RefuseConversion(SQLiteMetadataPath(cfg)); err != nil {
		return Set{}, err
	}
	if cfg.Metadata.Backend != "sqlite" {
		return OpenJSON(cfg.Runtime.RootDir), nil
	}
	path := SQLiteMetadataPath(cfg)
	engine, err := metasqlite.Open(path, sqliteDefinitions()...)
	if err != nil {
		return Set{}, fmt.Errorf("open configured metadata backend: %w", err)
	}
	vm := vm.NewWithEngine(cfg.Runtime.RootDir, engine)
	return Set{
		VM:         vm,
		Images:     image.NewWithEngine(cfg.Runtime.RootDir, engine),
		Snapshots:  snapshot.NewStoreWithEngineAndVMReader(cfg.Runtime.RootDir, engine, vm),
		Networks:   kbnetwork.NewStoreWithEngines(cfg.Runtime.RootDir, engine, engine, engine),
		OCI:        oci.NewStoreWithEngine(cfg.Runtime.RootDir, engine),
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
		{Name: "vms", Tables: []meta.Table{"vm-index"}},
		{Name: "images", Tables: []meta.Table{"image-index"}},
		{Name: "snapshots", Tables: []meta.Table{"snapshot-index"}},
		{Name: "networks", Tables: []meta.Table{"network-index"}},
		{Name: "leases", Tables: []meta.Table{"network-leases"}},
		{Name: "host-tap", Tables: []meta.Table{"host-tap"}},
		{Name: "oci-content", Tables: []meta.Table{"oci-content"}},
		{Name: "operations", Tables: []meta.Table{"records"}},
		{Name: "references", Tables: []meta.Table{"records"}},
		{Name: metering.Namespace, Tables: []meta.Table{metering.Table}},
	}
}

// OpenJSON creates the default JSON-backed resource stores.
func OpenJSON(rootDir string) Set {
	vm := vm.New(rootDir)
	return Set{
		VM:         vm,
		Images:     image.New(rootDir),
		Snapshots:  snapshot.NewStoreWithVMReader(rootDir, vm),
		Networks:   kbnetwork.NewStore(rootDir),
		OCI:        oci.NewStore(rootDir),
		Operations: operation.New(rootDir),
		References: reference.New(rootDir),
		Metering:   metering.New(rootDir),
		Guard:      lock.NewGuard(rootDir),
	}
}
