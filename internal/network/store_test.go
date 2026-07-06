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

func TestStoreInspectVMReportsDrift(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	rec := Record{
		ID:        "net_1",
		VMID:      "kb_1",
		Network:   "default",
		Provider:  ProviderHostTap,
		IfName:    "eth0",
		TAP:       "kbtap1",
		MAC:       "5a:00:00:00:00:01",
		BridgeDev: "kumabox0",
		IPs:       []string{"10.88.0.2/16"},
		Gateway:   "10.88.0.1",
		DNS:       []string{"1.1.1.1"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.UpsertRecord(rec); err != nil {
		t.Fatal(err)
	}

	result, err := store.InspectVM("kb_1", "p2", "default", []Config{{
		ID:        "net_1",
		TAP:       "kbtap1",
		MAC:       "5a:00:00:00:00:ff",
		Backend:   ProviderHostTap,
		BridgeDev: "kumabox0",
		Network: &GuestInfo{
			IP:      "10.88.0.2",
			Gateway: "10.88.0.1",
			Prefix:  16,
			DNS:     []string{"1.1.1.1"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.VMID != "kb_1" || result.VMName != "p2" || result.Network != "default" {
		t.Fatalf("unexpected inspect identity: %+v", result)
	}
	if len(result.Interfaces) != 1 || len(result.VMConfigs) != 1 {
		t.Fatalf("unexpected inspect payload: %+v", result)
	}
	if len(result.Drift) != 1 || result.Drift[0] != `network net_1 mac mismatch: vm="5a:00:00:00:00:ff" provider="5a:00:00:00:00:01"` {
		t.Fatalf("drift = %#v", result.Drift)
	}
}
