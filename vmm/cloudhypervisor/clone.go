package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var _ vmm.Cloner = (*Driver)(nil)

// Clone stages native snapshot state under the new sandbox's persistent VMM area.
// The immutable capture remains untouched while device paths and NICs are
// rebound to resources owned by the clone.
//
//	snapshot -> private copy -> patch paths -> restore paused -> swap NICs -> resume
func (d *Driver) Clone(ctx context.Context, plan vmm.ClonePlan) (vmm.Process, error) {
	if err := plan.Validate(); err != nil {
		return vmm.Process{}, err
	}
	if err := d.ValidateRestore(ctx, plan.SnapshotDir); err != nil {
		return vmm.Process{}, err
	}
	if err := d.paths.Prepare(plan.SandboxID); err != nil {
		return vmm.Process{}, err
	}
	privateDir, err := d.paths.CloneStateDir(plan.SandboxID)
	if err != nil {
		return vmm.Process{}, err
	}
	if err := storage.EnsureDir(filepath.Dir(privateDir)); err != nil {
		return vmm.Process{}, err
	}
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		return vmm.Process{}, fmt.Errorf("prepare private clone state: %w", err)
	}
	if err := copyNativeState(plan.SnapshotDir, privateDir); err != nil {
		return vmm.Process{}, err
	}
	apiSocket, err := d.paths.APISocket(plan.SandboxID)
	if err != nil {
		return vmm.Process{}, err
	}
	vsockSocket, err := d.paths.Vsock(plan.SandboxID)
	if err != nil {
		return vmm.Process{}, err
	}
	oldNets, err := patchCloneConfig(filepath.Join(privateDir, "config.json"), plan, vsockSocket)
	if err != nil {
		return vmm.Process{}, err
	}
	memoryMode := d.cloneMemoryMode(ctx, filepath.Join(privateDir, "config.json"))
	plan.SnapshotDir = privateDir
	return d.restore(ctx, plan.RestorePlan, memoryMode, func(ctx context.Context, _ string) error {
		if err := d.swapCloneNets(ctx, apiSocket, oldNets, plan.Network.Interfaces); err != nil {
			return err
		}
		return d.addCloneDataDisks(ctx, apiSocket, plan.NewDataDisks, plan.CPUs)
	})
}

// addCloneDataDisks attaches freshly formatted disks to the paused clone.
// They were absent from the captured device tree, so config patching alone
// cannot expose them to the restored guest.
func (d *Driver) addCloneDataDisks(ctx context.Context, socket string, disks []vmm.Disk, cpus uint32) error {
	for _, disk := range disks {
		payload, err := cloneDataDiskPayload(disk, cpus)
		if err != nil {
			return err
		}
		if err := d.snapshotAction(ctx, socket, "vm.add-disk", payload, d.startupTimeout); err != nil {
			return fmt.Errorf("add clone data disk %q: %w", disk.Serial, err)
		}
	}
	return nil
}

func cloneDataDiskPayload(disk vmm.Disk, cpus uint32) ([]byte, error) {
	direct := disk.DirectIO == nil || *disk.DirectIO
	return json.Marshal(map[string]any{
		"id": "kumabox-data-" + disk.Serial, "path": disk.Path, "serial": disk.Serial,
		"image_type": "Raw", "readonly": false, "direct": direct,
		"sparse": true, "num_queues": cpus, "queue_size": diskQueueSize,
	})
}

// copyNativeState shares immutable memory files by hard link when possible.
// Other files are copied; writable disks already live at sandbox-owned paths.
func copyNativeState(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "cow.raw" || strings.HasPrefix(entry.Name(), "data-") && strings.HasSuffix(entry.Name(), ".raw") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot entry %q is not a regular file", entry.Name())
		}
		from, to := filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())
		if err := cloneNativeFile(to, from, strings.HasPrefix(entry.Name(), "memory-range")); err != nil {
			return fmt.Errorf("copy snapshot entry %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func cloneNativeFile(destination, source string, immutableMemory bool) error {
	return cloneNativeFileWithLink(destination, source, immutableMemory, os.Link)
}

func cloneNativeFileWithLink(destination, source string, immutableMemory bool, link func(string, string) error) error {
	if immutableMemory {
		linkErr := link(source, destination)
		if linkErr == nil {
			return nil
		}
		if errors.Is(linkErr, syscall.EXDEV) {
			// The monitor opens the source before clone completes and keeps its
			// memory mapping alive even if the snapshot name is later removed.
			return os.Symlink(source, destination)
		}
	}
	return storage.CloneFile(destination, source)
}

type cloneNet struct {
	ID string `json:"id"`
}

// patchCloneConfig preserves unknown Cloud Hypervisor fields while replacing
// only source-owned paths. NICs first attach to clone-unique temporary TAPs;
// after vm.restore they are replaced with the newly allocated real NICs.
func patchCloneConfig(path string, plan vmm.ClonePlan, vsockSocket string) ([]cloneNet, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // private managed clone state
	if err != nil {
		return nil, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil || config == nil {
		return nil, errors.Join(err, errors.New("invalid native clone config"))
	}
	var disks []map[string]json.RawMessage
	if err := json.Unmarshal(config["disks"], &disks); err != nil || len(disks) == 0 {
		return nil, errors.Join(err, errors.New("clone config has no disks"))
	}
	if len(disks) != len(plan.ImageDisks)+1+len(plan.DataDisks) {
		return nil, fmt.Errorf("clone config has %d disks, expected %d", len(disks), len(plan.ImageDisks)+1+len(plan.DataDisks))
	}
	cowCount := 0
	for position, disk := range disks {
		var serial string
		if err := json.Unmarshal(disk["serial"], &serial); err != nil {
			return nil, fmt.Errorf("decode snapshot disk serial: %w", err)
		}
		switch {
		case serial == vmm.COWSerial:
			cowCount++
			disk["path"], err = json.Marshal(plan.WritableDisk)
			if err != nil {
				return nil, err
			}
			disk["readonly"] = json.RawMessage("false")
		case position < len(plan.ImageDisks):
			if serial != plan.ImageDisks[position].Serial {
				return nil, fmt.Errorf("clone image disk %d has unexpected serial %q", position, serial)
			}
			disk["path"], err = json.Marshal(plan.ImageDisks[position].Path)
			if err != nil {
				return nil, err
			}
			disk["readonly"] = json.RawMessage("true")
		default:
			dataIndex := position - len(plan.ImageDisks) - 1
			if dataIndex < 0 || dataIndex >= len(plan.DataDisks) || serial != plan.DataDisks[dataIndex].Serial {
				return nil, fmt.Errorf("clone data disk %d has unexpected serial %q", dataIndex, serial)
			}
			disk["path"], err = json.Marshal(plan.DataDisks[dataIndex].Path)
			if err != nil {
				return nil, err
			}
			disk["readonly"] = json.RawMessage("false")
			direct := plan.DataDisks[dataIndex].DirectIO == nil || *plan.DataDisks[dataIndex].DirectIO
			disk["direct"], err = json.Marshal(direct)
			if err != nil {
				return nil, err
			}
		}
	}
	if cowCount != 1 {
		return nil, fmt.Errorf("clone config requires one COW disk, found %d", cowCount)
	}
	config["disks"], err = json.Marshal(disks)
	if err != nil {
		return nil, err
	}
	if raw, ok := config["payload"]; ok && string(raw) != "null" {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
			return nil, errors.Join(err, errors.New("clone payload is invalid"))
		}
		if payload["kernel"], err = json.Marshal(plan.Kernel); err != nil {
			return nil, err
		}
		if payload["initramfs"], err = json.Marshal(plan.Initrd); err != nil {
			return nil, err
		}
		if config["payload"], err = json.Marshal(payload); err != nil {
			return nil, err
		}
	}
	var vsock map[string]json.RawMessage
	if err := json.Unmarshal(config["vsock"], &vsock); err != nil || vsock == nil {
		return nil, errors.Join(err, errors.New("clone config has no vsock"))
	}
	vsock["socket"], err = json.Marshal(vsockSocket)
	if err != nil {
		return nil, err
	}
	config["vsock"], err = json.Marshal(vsock)
	if err != nil {
		return nil, err
	}
	var nets []map[string]json.RawMessage
	if value, exists := config["net"]; exists && string(value) != "null" {
		if err := json.Unmarshal(value, &nets); err != nil {
			return nil, fmt.Errorf("decode snapshot NICs: %w", err)
		}
	}
	oldNets := make([]cloneNet, len(nets))
	for index, device := range nets {
		if err := json.Unmarshal(device["id"], &oldNets[index].ID); err != nil || oldNets[index].ID == "" {
			return nil, errors.Join(err, fmt.Errorf("snapshot NIC %d has no removable ID", index))
		}
		tap, err := network.TAPName("rt", plan.SandboxID, index)
		if err != nil {
			return nil, err
		}
		device["tap"], err = json.Marshal(tap)
		if err != nil {
			return nil, err
		}
	}
	if len(nets) > 0 {
		config["net"], err = json.Marshal(nets)
		if err != nil {
			return nil, err
		}
	}
	patched, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil { //nolint:gosec // private managed clone state
		return nil, err
	}
	return oldNets, nil
}

// swapCloneNets runs while the VM is paused, before guest memory can send
// traffic with the source MAC or IP. A failed swap aborts the new VMM.
func (d *Driver) swapCloneNets(ctx context.Context, socket string, old []cloneNet, fresh []types.NetworkInterface) error {
	for _, device := range old {
		payload, err := json.Marshal(map[string]string{"id": device.ID})
		if err != nil {
			return err
		}
		if err := d.snapshotAction(ctx, socket, "vm.remove-device", payload, d.startupTimeout); err != nil {
			return fmt.Errorf("remove source NIC %s: %w", device.ID, err)
		}
	}
	for _, device := range fresh {
		payload, err := json.Marshal(map[string]any{
			"id": "kumabox-net-" + fmt.Sprint(device.Index), "tap": device.TAP, "mac": device.MAC,
			"num_queues": device.Queues, "queue_size": device.QueueSize,
			"offload_tso": true, "offload_ufo": true, "offload_csum": true,
		})
		if err != nil {
			return err
		}
		if err := d.snapshotAction(ctx, socket, "vm.add-net", payload, d.startupTimeout); err != nil {
			return fmt.Errorf("add clone NIC %d: %w", device.Index, err)
		}
	}
	return nil
}
