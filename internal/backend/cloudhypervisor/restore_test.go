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
	if _, err := patchRestoreConfig(path, rec, false); err != nil {
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

func TestPatchRestoreConfigRebindsCloneTapWithoutChangingGuestIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
  "disks":[{"path":"/old/cow.raw"}],
  "net":[{"id":"snapshot-net0","tap":"kbtapsource","mac":"02:00:00:00:00:01","num_queues":2,"queue_size":256}]
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &vmstore.VMRecord{
		StorageConfigs: []vmstore.StorageConfig{{ID: "cow", Path: "/new/cow.raw"}},
		NetworkConfigs: []kbnetwork.Config{{TAP: "kbtapclone", MAC: "02:00:00:00:00:02"}},
	}
	patched, err := patchRestoreConfig(path, rec, true)
	if err != nil {
		t.Fatal(err)
	}
	var nets []map[string]any
	if err := json.Unmarshal(patched["net"], &nets); err != nil {
		t.Fatal(err)
	}
	if len(nets) != 1 || nets[0]["tap"] != "kbtapclone" {
		t.Fatalf("patched networks = %#v", nets)
	}
	if nets[0]["id"] != "snapshot-net0" || nets[0]["mac"] != "02:00:00:00:00:01" {
		t.Fatalf("snapshot guest identity changed before restore: %#v", nets[0])
	}
}

func TestHotSwapCloneNetworksRemovesOldBeforeAddingNew(t *testing.T) {
	var calls []string
	state := "Paused"
	oldDevicePresent := true
	ejectObserved := false
	client := apiTestClient(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		calls = append(calls, req.URL.Path+":"+string(body))
		switch req.URL.Path {
		case "/api/v1/vm.info":
			deviceTree := map[string]any{}
			if oldDevicePresent {
				deviceTree["old-net"] = map[string]any{"id": "old-net"}
			}
			payload, _ := json.Marshal(map[string]any{"state": state, "device_tree": deviceTree})
			if state == "Running" && oldDevicePresent {
				if ejectObserved {
					oldDevicePresent = false
				} else {
					ejectObserved = true
				}
			}
			return apiResponse(http.StatusOK, string(payload)), nil
		case "/api/v1/vm.resume":
			state = "Running"
			return apiResponse(http.StatusNoContent, ""), nil
		case "/api/v1/vm.pause":
			state = "Paused"
			return apiResponse(http.StatusNoContent, ""), nil
		}
		code := http.StatusNoContent
		if req.URL.Path == "/api/v1/vm.add-net" {
			if oldDevicePresent || state != "Paused" {
				t.Fatalf("new NIC added before eject barrier: present=%t state=%s", oldDevicePresent, state)
			}
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
	if len(calls) < 8 || calls[0] != `/api/v1/vm.remove-device:{"id":"old-net"}` {
		t.Fatalf("calls = %v", calls)
	}
	wantID := cloneNetworkDeviceID(rec.NetworkConfigs[0].MAC)
	got := calls[len(calls)-1]
	if !strings.Contains(got, "/api/v1/vm.add-net:") || !strings.Contains(got, fmt.Sprintf(`"id":"%s"`, wantID)) || !strings.Contains(got, `"tap":"kbtapnew"`) {
		t.Fatalf("add call = %s", got)
	}
}

func TestNativeRestoreRequestMapsMemoryModes(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{mode: "copy", want: ""},
		{mode: "ondemand", want: "OnDemand"},
		{mode: "mmap", want: "Mmap"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			request, err := nativeRestoreRequest("/tmp/snapshot with space", tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			if request.MemoryRestoreMode != tt.want || request.SourceURL != "file:///tmp/snapshot%20with%20space" {
				t.Fatalf("request = %+v", request)
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if tt.mode == "copy" && strings.Contains(string(raw), "memory_restore_mode") {
				t.Fatalf("copy request contains extension: %s", raw)
			}
		})
	}
	if _, err := nativeRestoreRequest("/tmp/snapshot", "invalid"); err == nil {
		t.Fatal("expected unsupported mode error")
	}
}
