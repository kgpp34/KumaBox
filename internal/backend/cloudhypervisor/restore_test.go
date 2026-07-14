package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestPatchRestoreConfigPreservesBackendFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
  "platform":{"num_pci_segments":1},
  "disks":[{"path":"/old/cow.raw","readonly":false,"id":"disk0","queue_size":128}],
  "serial":{"mode":"File","file":"/old/console.log"},
  "vsock":{"cid":3,"socket":"/old/vsock.sock","id":"vsock0"}
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &vmstore.VMRecord{
		LogDir: "/new/log", VsockSocket: "/new/vsock.sock",
		StorageConfigs: []vmstore.StorageConfig{{ID: "cow", Path: "/new/cow.raw"}},
	}
	if err := patchRestoreConfig(path, rec); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	patched, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["platform"]; !ok {
		t.Fatal("platform field was discarded")
	}
	var disks []map[string]any
	if err := json.Unmarshal(got["disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if disks[0]["path"] != "/new/cow.raw" || disks[0]["id"] != "disk0" || disks[0]["queue_size"] != float64(128) {
		t.Fatalf("patched disks = %#v", disks)
	}
	var vsock map[string]any
	if err := json.Unmarshal(got["vsock"], &vsock); err != nil {
		t.Fatal(err)
	}
	if vsock["socket"] != rec.VsockSocket || vsock["id"] != "vsock0" {
		t.Fatalf("patched vsock = %#v", vsock)
	}
}
