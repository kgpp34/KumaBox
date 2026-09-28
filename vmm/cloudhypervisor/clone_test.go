package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestPatchCloneConfigRebindsOnlyPrivateDevices(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	original := `{
		"platform":{"firmware":"preserved"},
		"disks":[{"serial":"kumabox-layer0","path":"/images/base.raw","readonly":true},{"serial":"kumabox-cow","path":"/source/cow.raw","direct":true}],
		"vsock":{"cid":3,"socket":"/source/vsock.uds"},
		"net":[{"id":"old-nic","tap":"source-tap","mac":"02:00:00:00:00:01","num_queues":4}]
	}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := vmm.ClonePlan{
		RestorePlan: vmm.RestorePlan{
			SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
			Network: types.NetworkSetup{Interfaces: []types.NetworkInterface{{
				Index: 0, Name: "eth0", TAP: "new-tap", MAC: "02:00:00:00:00:02",
				Queues: 4, QueueSize: 512, Network: "test",
			}}},
		},
		WritableDisk: "/clone/cow.raw",
	}
	old, err := patchCloneConfig(path, plan, "/clone/vsock.uds")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].ID != "old-nic" {
		t.Fatalf("old NICs = %+v", old)
	}
	patched, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Platform json.RawMessage `json:"platform"`
		Disks    []struct {
			Path string `json:"path"`
		} `json:"disks"`
		Vsock struct {
			Socket string `json:"socket"`
		} `json:"vsock"`
		Nets []struct {
			ID  string `json:"id"`
			TAP string `json:"tap"`
			MAC string `json:"mac"`
		} `json:"net"`
	}
	if err := json.Unmarshal(patched, &config); err != nil {
		t.Fatal(err)
	}
	if config.Disks[0].Path != "/images/base.raw" || config.Disks[1].Path != "/clone/cow.raw" ||
		config.Vsock.Socket != "/clone/vsock.uds" || config.Nets[0].TAP == "source-tap" ||
		config.Nets[0].MAC != "02:00:00:00:00:01" || string(config.Platform) != `{"firmware":"preserved"}` {
		t.Fatalf("patched config = %s", patched)
	}
}

func TestCrossFilesystemMemoryUsesSourceLink(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "memory-range-0")
	target := filepath.Join(directory, "clone-memory")
	if err := os.WriteFile(source, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cloneNativeFileWithLink(target, source, true, func(_, _ string) error { return syscall.EXDEV }); err != nil {
		t.Fatal(err)
	}
	if destination, err := os.Readlink(target); err != nil || destination != source {
		t.Fatalf("cross-filesystem memory link = %q, %v", destination, err)
	}
}

func TestPatchCloneConfigRejectsUnidentifiedNIC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"disks":[{"serial":"kumabox-cow","path":"/old"}],"vsock":{"socket":"/old"},"net":[{"tap":"old"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := vmm.ClonePlan{RestorePlan: vmm.RestorePlan{
		SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		Network:   types.NetworkSetup{Interfaces: []types.NetworkInterface{{Index: 0}}},
	}, WritableDisk: "/new/cow.raw"}
	if _, err := patchCloneConfig(path, plan, "/new/vsock.uds"); err == nil {
		t.Fatal("patch accepted NIC without a removable device ID")
	}
}

func TestCopyNativeStateKeepsCaptureAndSkipsWritableDisk(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "native")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"config.json": `{"original":true}`, "state.json": `{"version":1}`,
		"memory-range-0": "memory", "cow.raw": "private disk",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyNativeState(source, target); err != nil {
		t.Fatal(err)
	}
	memorySource, err := os.Stat(filepath.Join(source, "memory-range-0"))
	if err != nil {
		t.Fatal(err)
	}
	memoryTarget, err := os.Stat(filepath.Join(target, "memory-range-0"))
	if err != nil || !os.SameFile(memorySource, memoryTarget) {
		t.Fatalf("local memory snapshot was copied instead of shared: %v", err)
	}
	if err := os.Remove(filepath.Join(source, "memory-range-0")); err != nil {
		t.Fatal(err)
	}
	memory, err := os.ReadFile(filepath.Join(target, "memory-range-0"))
	if err != nil || string(memory) != "memory" {
		t.Fatalf("clone lost its shared memory file after source removal: %q, %v", memory, err)
	}
	if _, err := os.Stat(filepath.Join(target, "cow.raw")); !os.IsNotExist(err) {
		t.Fatalf("native copy contains writable disk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "config.json"), []byte(`{"clone":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err != nil || string(original) != `{"original":true}` {
		t.Fatalf("source capture changed: %s, %v", original, err)
	}
}
