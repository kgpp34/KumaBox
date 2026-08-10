package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/meta"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// ConvertJSONToSQLite imports existing JSON resource indexes into one SQLite
// database. Source files remain untouched, so a failed conversion can be
// retried or rolled back by selecting the JSON backend again.
func ConvertJSONToSQLite(ctx context.Context, rootDir, databasePath string) (statuses []metasqlite.NamespaceStatus, err error) {
	if databasePath == "" {
		databasePath = filepath.Join(rootDir, "metadata", "kumabox.db")
	}
	if _, statErr := os.Stat(databasePath); errors.Is(statErr, os.ErrNotExist) {
		if err := metasqlite.Init(ctx, databasePath, sqliteDefinitions()...); err != nil {
			return nil, fmt.Errorf("initialize sqlite conversion target: %w", err)
		}
	} else if statErr != nil {
		return nil, fmt.Errorf("stat sqlite conversion target: %w", statErr)
	}
	destination, err := metasqlite.Open(databasePath, sqliteDefinitions()...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := destination.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close SQLite metadata store: %w", closeErr)
		}
	}()

	vm := vmstore.New(rootDir)
	images := imagestore.New(rootDir)
	snapshots := snapshot.NewStore(rootDir)
	networks := kbnetwork.NewStore(rootDir)
	oci := ocistore.New(rootDir)
	operations := operation.New(rootDir)
	references := reference.New(rootDir)
	engines := []meta.MetaEngine{
		vm.MetadataEngine(), images.MetadataEngine(), snapshots.MetadataEngine(),
		oci.MetadataEngine(), operations.MetadataEngine(), references.MetadataEngine(),
	}
	defer func() {
		for _, engine := range engines {
			_ = engine.Close()
		}
	}()
	networkEngine, leaseEngine, hostTapEngine := networks.MetadataEngines()
	defer func() {
		if closeErr := networkEngine.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close network metadata store: %w", closeErr)
		}
	}()
	defer func() {
		if closeErr := leaseEngine.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close lease metadata store: %w", closeErr)
		}
	}()
	defer func() {
		if closeErr := hostTapEngine.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close host-tap metadata store: %w", closeErr)
		}
	}()

	conversions := []struct {
		engine meta.MetaEngine
		set    meta.TableSet
	}{
		{vm.MetadataEngine(), meta.TableSet{Namespace: "vms", Tables: []meta.Table{"vm-index"}}},
		{images.MetadataEngine(), meta.TableSet{Namespace: "images", Tables: []meta.Table{"image-index"}}},
		{snapshots.MetadataEngine(), meta.TableSet{Namespace: "snapshots", Tables: []meta.Table{"snapshot-index"}}},
		{networkEngine, meta.TableSet{Namespace: "networks", Tables: []meta.Table{"network-index"}}},
		{leaseEngine, meta.TableSet{Namespace: "leases", Tables: []meta.Table{"network-leases"}}},
		{hostTapEngine, meta.TableSet{Namespace: "host-tap", Tables: []meta.Table{"host-tap"}}},
		{oci.MetadataEngine(), meta.TableSet{Namespace: "oci-content", Tables: []meta.Table{"oci-content"}}},
		{operations.MetadataEngine(), meta.TableSet{Namespace: "operations", Tables: []meta.Table{"records"}}},
		{references.MetadataEngine(), meta.TableSet{Namespace: "references", Tables: []meta.Table{"records"}}},
	}
	for _, conversion := range conversions {
		if _, err := metasqlite.Convert(ctx, conversion.engine, destination, "json", []meta.TableSet{conversion.set}); err != nil {
			return nil, fmt.Errorf("convert namespace %s: %w", conversion.set.Namespace, err)
		}
	}
	return destination.Status(ctx)
}
