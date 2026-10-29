package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

func TestPatchCloneConfigRebindsOnlyPrivateDevices(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	original := `{
		"platform":{"firmware":"preserved"},
		"payload":{"kernel":"/source/kernel","initramfs":"/source/initrd","cmdline":"preserved"},
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
		ImageDisks:   []vmm.Disk{{Path: "/target/images/base.raw", Serial: vmm.LayerSerialPrefix + "0", ReadOnly: true}},
		Kernel:       "/target/kernel",
		Initrd:       "/target/initrd",
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
		Payload  struct {
			Kernel  string `json:"kernel"`
			Initrd  string `json:"initramfs"`
			Cmdline string `json:"cmdline"`
		} `json:"payload"`
		Disks []struct {
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
	if config.Disks[0].Path != "/target/images/base.raw" || config.Disks[1].Path != "/clone/cow.raw" ||
		config.Vsock.Socket != "/clone/vsock.uds" || config.Nets[0].TAP == "source-tap" ||
		config.Nets[0].MAC != "02:00:00:00:00:01" || string(config.Platform) != `{"firmware":"preserved"}` ||
		config.Payload.Kernel != "/target/kernel" || config.Payload.Initrd != "/target/initrd" || config.Payload.Cmdline != "preserved" {
		t.Fatalf("patched config = %s", patched)
	}
}

func TestPatchCloneConfigPreservesFixedGuestNIC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"disks":[{"serial":"kumabox-layer0","path":"/old/base"},{"serial":"kumabox-cow","path":"/old/cow"}],"vsock":{"socket":"/old/vsock"},"net":[{"id":"captured-nic","tap":"old-tap","mac":"02:4b:42:00:00:02","num_queues":4,"queue_size":512}]}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := vmm.ClonePlan{RestorePlan: vmm.RestorePlan{SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"), Network: types.NetworkSetup{
		Backend: types.NetworkBackendEBPF, Interfaces: []types.NetworkInterface{{Index: 0, TAP: "kt12345678-0", MAC: "02:4b:42:00:00:02", Queues: 4, QueueSize: 512, AddressMode: types.AddressModeFixed}},
	}}, WritableDisk: "/new/cow", ImageDisks: []vmm.Disk{{Path: "/new/base", Serial: "kumabox-layer0"}}}
	old, err := patchCloneConfig(path, plan, "/new/vsock")
	if err != nil {
		t.Fatal(err)
	}
	if old != nil {
		t.Fatalf("fixed NIC requires no remove/add: %+v", old)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var patched struct {
		Net []struct{ ID, TAP, MAC string }
	}
	if err := json.Unmarshal(data, &patched); err != nil {
		t.Fatal(err)
	}
	if len(patched.Net) != 1 || patched.Net[0].ID != "captured-nic" || patched.Net[0].TAP != "kt12345678-0" || patched.Net[0].MAC != "02:4b:42:00:00:02" {
		t.Fatalf("guest device was changed: %+v", patched)
	}
}

func TestPatchCloneConfigRebindsManagedDataDisk(t *testing.T) {
	directIO := false
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"disks":[{"serial":"kumabox-layer0","path":"/old/layer"},{"serial":"kumabox-cow","path":"/old/cow"},{"serial":"db","path":"/old/data-db.raw","direct":true}],"vsock":{"socket":"/old/vsock"}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := vmm.ClonePlan{
		RestorePlan:  vmm.RestorePlan{SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000")},
		WritableDisk: "/new/cow", ImageDisks: []vmm.Disk{{Path: "/new/layer", Serial: "kumabox-layer0", ReadOnly: true}},
		DataDisks: []vmm.Disk{{Path: "/new/data-db.raw", Serial: "db", DirectIO: &directIO}}, Kernel: "/new/kernel", Initrd: "/new/initrd",
	}
	if _, err := patchCloneConfig(path, plan, "/new/vsock"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Disks []struct {
			Path     string `json:"path"`
			ReadOnly bool   `json:"readonly"`
			Direct   bool   `json:"direct"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Disks) != 3 || config.Disks[2].Path != "/new/data-db.raw" || config.Disks[2].ReadOnly || config.Disks[2].Direct {
		t.Fatalf("managed data disk was not rebound: %s", raw)
	}
}

func TestCloneDataDiskPayloadHonorsDirectIO(t *testing.T) {
	off := false
	payload, err := cloneDataDiskPayload(vmm.Disk{Path: "/clone/data-new.raw", Serial: "new", DirectIO: &off}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Direct bool   `json:"direct"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "kumabox-data-new" || decoded.Path != "/clone/data-new.raw" || decoded.Direct {
		t.Fatalf("clone data disk payload = %s", payload)
	}
}

func TestCloneRootDiskIOOverridesCapturedPolicy(t *testing.T) {
	for _, mode := range []types.DiskIOMode{"", types.DiskIOAuto, types.DiskIODirect, types.DiskIOBuffered} {
		name := string(mode)
		if name == "" {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			original := `{"disks":[{"serial":"kumabox-layer0","path":"/old/layer","readonly":true},{"serial":"kumabox-cow","path":"/old/cow","direct":false}],"vsock":{"socket":"/old/vsock"}}`
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			plan := vmm.ClonePlan{
				RestorePlan:  vmm.RestorePlan{SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000")},
				WritableDisk: "/new/cow", COWIOMode: mode,
				ImageDisks: []vmm.Disk{{Path: "/new/layer", Serial: "kumabox-layer0", ReadOnly: true}},
			}
			if _, err := patchCloneConfig(path, plan, "/new/vsock"); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Disks []struct {
					Direct   bool
					Readonly bool
				}
			}
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			if len(config.Disks) != 2 || !config.Disks[0].Readonly || config.Disks[1].Direct != mode.DirectIO() {
				t.Fatalf("root cache policy or immutable layer changed: %s", raw)
			}
		})
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
	if err := os.WriteFile(path, []byte(`{"disks":[{"serial":"kumabox-layer0","path":"/old/layer"},{"serial":"kumabox-cow","path":"/old"}],"vsock":{"socket":"/old"},"net":[{"tap":"old"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := vmm.ClonePlan{RestorePlan: vmm.RestorePlan{
		SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		Network:   types.NetworkSetup{Interfaces: []types.NetworkInterface{{Index: 0}}},
	}, WritableDisk: "/new/cow.raw", ImageDisks: []vmm.Disk{{Path: "/new/layer", Serial: vmm.LayerSerialPrefix + "0", ReadOnly: true}}, Kernel: "/new/kernel", Initrd: "/new/initrd"}
	if _, err := patchCloneConfig(path, plan, "/new/vsock.uds"); err == nil {
		t.Fatal("patch accepted NIC without a removable device ID")
	}
}

func TestPatchCloneConfigAllowsNICCountChange(t *testing.T) {
	for _, targetCount := range []int{0, 2} {
		path := filepath.Join(t.TempDir(), "config.json")
		config := `{"disks":[{"serial":"kumabox-layer0","path":"/old/layer"},{"serial":"kumabox-cow","path":"/old/cow"}],"vsock":{"socket":"/old/vsock"},"net":[{"id":"old-nic","tap":"old-tap"}]}`
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		interfaces := make([]types.NetworkInterface, targetCount)
		plan := vmm.ClonePlan{RestorePlan: vmm.RestorePlan{
			SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
			Network:   types.NetworkSetup{Interfaces: interfaces},
		}, WritableDisk: "/new/cow.raw", ImageDisks: []vmm.Disk{{Path: "/new/layer", Serial: vmm.LayerSerialPrefix + "0", ReadOnly: true}}, Kernel: "/new/kernel", Initrd: "/new/initrd"}
		old, err := patchCloneConfig(path, plan, "/new/vsock.uds")
		if err != nil || len(old) != 1 || old[0].ID != "old-nic" {
			t.Fatalf("target NICs %d: old = %+v, %v", targetCount, old, err)
		}
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

func TestCloneNICIDIsUniqueAcrossGenerations(t *testing.T) {
	first := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	second := types.SandboxID("223e4567-e89b-42d3-a456-426614174000")
	left, right := cloneNICID(first, 0), cloneNICID(second, 0)
	if left == right {
		t.Fatalf("clone NIC IDs collided: %q", left)
	}
	if cloneNICID(first, 0) == cloneNICID(first, 1) {
		t.Fatal("NIC index did not affect clone ID")
	}
}
