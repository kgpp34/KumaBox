package resources

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/imagestore"
	metasqlite "github.com/kumabox/kumabox/internal/metastore/sqlite"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestNewStoreSetForConfigUsesOneSQLiteEngine(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.RootDir = t.TempDir()
	cfg.Metadata.Backend = "sqlite"
	if err := InitSQLiteMetadata(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	stores, err := NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stores.Metadata == nil || stores.VM == nil || stores.Images == nil || stores.Snapshots == nil || stores.Networks == nil || stores.OCI == nil || stores.Operations == nil || stores.Metering == nil {
		t.Fatalf("incomplete store set: %+v", stores)
	}
	statusStore, ok := stores.Metadata.(*metasqlite.Store)
	if !ok {
		t.Fatalf("metadata engine type = %T", stores.Metadata)
	}
	status, err := statusStore.Status(context.Background())
	if err != nil || len(status) != len(sqliteDefinitions()) {
		t.Fatalf("namespace status = %d, err = %v", len(status), err)
	}
	if err := stores.Metadata.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConvertJSONToSQLiteCreatesCompletedNamespaceState(t *testing.T) {
	root := t.TempDir()
	status, err := ConvertJSONToSQLite(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != len(sqliteDefinitions()) {
		t.Fatalf("converted namespace count = %d", len(status))
	}
	for _, namespace := range status {
		if namespace.State != "converted" || namespace.Source != "json" {
			t.Fatalf("namespace conversion status = %+v", namespace)
		}
	}
}

func TestConvertMetadataRoundTripsJSONAndSQLite(t *testing.T) {
	cfg := testMetadataConfig(t)
	seedJSONMetadata(t, cfg.Runtime.RootDir)

	cfg.Metadata.Backend = "sqlite"
	toSQLite, err := ConvertMetadata(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if toSQLite.Backend != "sqlite" || len(toSQLite.Namespaces) != len(sqliteDefinitions()) {
		t.Fatalf("sqlite conversion result = %+v", toSQLite)
	}
	assertConvertedRecords(t, cfg)

	cfg.Metadata.Backend = "json"
	toJSON, err := ConvertMetadata(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if toJSON.Backend != "json" || len(toJSON.Namespaces) != len(sqliteDefinitions()) {
		t.Fatalf("json conversion result = %+v", toJSON)
	}
	assertConvertedRecords(t, cfg)
}

func TestConvertMetadataResumesAfterCommittedNamespace(t *testing.T) {
	cfg := testMetadataConfig(t)
	seedJSONMetadata(t, cfg.Runtime.RootDir)
	cfg.Metadata.Backend = "sqlite"

	injected := errors.New("injected conversion interruption")
	ctx := fault.WithInjector(t.Context(), fault.InjectorFunc(func(point fault.Point) error {
		if point == fault.MetadataConvertNamespace {
			return injected
		}
		return nil
	}))
	_, err := ConvertMetadata(ctx, cfg)
	if !errors.Is(err, injected) {
		t.Fatalf("interrupted conversion error = %v", err)
	}
	if _, err := NewStoreSetForConfig(cfg); err == nil {
		t.Fatal("ordinary store open succeeded while conversion manifest existed")
	}
	if _, err := ConvertMetadata(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	assertConvertedRecords(t, cfg)
}

func TestConvertMetadataResumesWhileRetiringSQLiteSource(t *testing.T) {
	cfg := testMetadataConfig(t)
	seedJSONMetadata(t, cfg.Runtime.RootDir)
	cfg.Metadata.Backend = "sqlite"
	if _, err := ConvertMetadata(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}

	cfg.Metadata.Backend = "json"
	injected := errors.New("injected source retirement interruption")
	ctx := fault.WithInjector(t.Context(), fault.InjectorFunc(func(point fault.Point) error {
		if point == fault.MetadataConvertRetired {
			return injected
		}
		return nil
	}))
	_, err := ConvertMetadata(ctx, cfg)
	if !errors.Is(err, injected) {
		t.Fatalf("interrupted retirement error = %v", err)
	}
	if _, err := ConvertMetadata(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	assertConvertedRecords(t, cfg)
}

func testMetadataConfig(t *testing.T) config.Config {
	t.Helper()
	rootDir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(rootDir, "run")
	cfg.Runtime.LogDir = filepath.Join(rootDir, "log")
	return cfg
}

func seedJSONMetadata(t *testing.T, rootDir string) {
	t.Helper()
	stores := NewStoreSet(rootDir)
	if _, err := stores.Operations.Begin(t.Context(), "op-convert", operation.KindVMStart, "vm-convert"); err != nil {
		t.Fatal(err)
	}
	if err := stores.References.Upsert(t.Context(), reference.Record{
		ID: "ref-convert", SourceKind: "vm", SourceID: "vm-convert", TargetKind: "image", TargetID: "image-convert",
	}); err != nil {
		t.Fatal(err)
	}
	closeJSONStoreSet(t, stores)
}

func assertConvertedRecords(t *testing.T, cfg config.Config) {
	t.Helper()
	stores, err := NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metadata.Backend == "sqlite" {
		defer func() {
			if err := stores.Metadata.Close(); err != nil {
				t.Errorf("close sqlite metadata: %v", err)
			}
		}()
	} else {
		defer closeJSONStoreSet(t, stores)
	}
	recoverable, err := stores.Operations.Recoverable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != "op-convert" {
		t.Fatalf("converted operations = %+v", recoverable)
	}
	references, err := stores.References.ListTarget(t.Context(), "image", "image-convert")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ID != "ref-convert" {
		t.Fatalf("converted references = %+v", references)
	}
}

func closeJSONStoreSet(t *testing.T, stores StoreSet) {
	t.Helper()
	engines := []interface{ Close() error }{
		stores.VM.(*vmstore.Store).MetadataEngine(),
		stores.Images.(*imagestore.Store).MetadataEngine(),
		stores.Snapshots.(*snapshot.Store).MetadataEngine(),
		stores.OCI.(*ocistore.Store).MetadataEngine(),
		stores.Operations.(*operation.Journal).MetadataEngine(),
		stores.References.(*reference.Store).MetadataEngine(),
	}
	network, leases, hostTap := stores.Networks.(*kbnetwork.Store).MetadataEngines()
	engines = append(engines, network, leases, hostTap)
	for _, engine := range engines {
		if err := engine.Close(); err != nil {
			t.Errorf("close json metadata: %v", err)
		}
	}
}
