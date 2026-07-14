package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
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
	if _, err := patchRestoreConfig(path, rec); err != nil {
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

func TestHotSwapCloneNetworksRemovesOldBeforeAddingNew(t *testing.T) {
	var calls []string
	client := apiTestClient(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		calls = append(calls, req.URL.Path+":"+string(body))
		code := http.StatusNoContent
		if req.URL.Path == "/api/v1/vm.add-net" {
			code = http.StatusOK
		}
		return apiResponse(code, ""), nil
	})
	old, err := json.Marshal([]map[string]any{{"id": "old-net", "mac": "02:00:00:00:00:01"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &vmstore.VMRecord{NetworkConfigs: []kbnetwork.Config{{
		TAP: "kbtapnew", MAC: "02:00:00:00:00:02", NumQueues: 2, QueueSize: 256,
	}}}
	if err := hotSwapCloneNetworks(context.Background(), client, map[string]json.RawMessage{"net": old}, rec); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != `/api/v1/vm.remove-device:{"id":"old-net"}` {
		t.Fatalf("calls = %v", calls)
	}
	wantID := cloneNetworkDeviceID(rec.NetworkConfigs[0].MAC)
	if got := calls[1]; !strings.Contains(got, "/api/v1/vm.add-net:") || !strings.Contains(got, fmt.Sprintf(`"id":"%s"`, wantID)) || !strings.Contains(got, `"tap":"kbtapnew"`) {
		t.Fatalf("add call = %s", got)
	}
}
