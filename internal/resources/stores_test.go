package resources

import (
	"context"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
)

func TestNewStoreSetForConfigUsesOneSQLiteEngine(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.RootDir = t.TempDir()
	cfg.Metadata.Backend = "sqlite"
	stores, err := NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stores.Metadata == nil || stores.VM == nil || stores.Images == nil || stores.Snapshots == nil || stores.Networks == nil || stores.OCI == nil || stores.Operations == nil {
		t.Fatalf("incomplete store set: %+v", stores)
	}
	statusStore, ok := stores.Metadata.(*metasqlite.Store)
	if !ok {
		t.Fatalf("metadata engine type = %T", stores.Metadata)
	}
	status, err := statusStore.Status(context.Background())
	if err != nil || len(status) != 9 {
		t.Fatalf("namespace status = %d, err = %v", len(status), err)
	}
	if err := stores.Metadata.Close(); err != nil {
		t.Fatal(err)
	}
}
