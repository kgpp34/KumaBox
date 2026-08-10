package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
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
