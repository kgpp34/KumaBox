package network

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreListMissingIndexReturnsEmpty(t *testing.T) {
	store := NewStore(t.TempDir())
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %d, want 0", len(records))
	}
}

func TestStoreListReadsNetworkIndex(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "network", "index.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
  "schemaVersion": "kumabox.network.index.v1",
  "networks": {
    "net_b": {
      "id": "net_b",
      "vmId": "kb_b",
      "network": "default",
      "provider": "host-tap",
      "ifName": "eth0",
      "tap": "kbtapb",
      "mac": "02:00:00:00:00:02",
      "createdAt": "2026-06-29T00:00:02Z",
      "updatedAt": "2026-06-29T00:00:02Z",
      "cleanup": {"pending": false}
    },
    "net_a": {
      "id": "net_a",
      "vmId": "kb_a",
      "network": "default",
      "provider": "host-tap",
      "ifName": "eth0",
      "tap": "kbtapa",
      "mac": "02:00:00:00:00:01",
      "createdAt": "2026-06-29T00:00:01Z",
      "updatedAt": "2026-06-29T00:00:01Z",
      "cleanup": {"pending": false}
    }
  }
}`)
	if err := os.WriteFile(indexPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := NewStore(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].ID != "net_a" || records[1].ID != "net_b" {
		t.Fatalf("records not sorted by creation time: %+v", records)
	}
	if records[0].CreatedAt.IsZero() || !records[0].CreatedAt.Equal(time.Date(2026, 6, 29, 0, 0, 1, 0, time.UTC)) {
		t.Fatalf("createdAt = %s", records[0].CreatedAt)
	}
}
