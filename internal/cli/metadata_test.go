package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	metasqlite "github.com/kumabox/kumabox/internal/metastore/sqlite"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/resources"
)

func TestMetadataInitCommandCreatesVerifiedSQLiteStore(t *testing.T) {
	rootDir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(rootDir, "run")
	cfg.Runtime.LogDir = filepath.Join(rootDir, "log")
	cfg.Metadata.Backend = "sqlite"

	cmd := NewRootCommandWithConfig(cfg)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"metadata", "init"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Initialized bool `json:"initialized"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Initialized {
		t.Fatal("metadata init did not report success")
	}

	stores, err := resources.NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine, ok := stores.Metadata.(*metasqlite.Store)
	if !ok {
		t.Fatalf("metadata engine type = %T", stores.Metadata)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close metadata engine: %v", err)
		}
	})
	if err := engine.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataBackupCommandCreatesUsableDatabase(t *testing.T) {
	rootDir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(rootDir, "run")
	cfg.Runtime.LogDir = filepath.Join(rootDir, "log")
	cfg.Metadata.Backend = "sqlite"
	if err := resources.InitSQLiteMetadata(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	stores, err := resources.NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.References.Upsert(t.Context(), reference.Record{
		ID: "backup-ref", SourceKind: "vm", SourceID: "vm-1", TargetKind: "image", TargetID: "image-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Metadata.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "kumabox-backup.db")
	cmd := NewRootCommandWithConfig(cfg)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"metadata", "backup", destination})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Verified {
		t.Fatal("metadata backup did not report verification")
	}

	backupConfig := cfg
	backupConfig.Metadata.Path = destination
	backupStores, err := resources.NewStoreSetForConfig(backupConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := backupStores.Metadata.Close(); err != nil {
			t.Errorf("close backup metadata: %v", err)
		}
	}()
	references, err := backupStores.References.ListTarget(t.Context(), "image", "image-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ID != "backup-ref" {
		t.Fatalf("backup references = %+v", references)
	}
}
