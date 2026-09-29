package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const nicEjectTimeout = 20 * time.Second

var _ vmm.NetworkHotplugger = (*Driver)(nil)

// liveNetworkInfo verifies the owned process before using its private API.
func (d *Driver) liveNetworkInfo(ctx context.Context, process vmm.Process) (vmInfo, error) {
	if err := process.Validate(); err != nil {
		return vmInfo{}, err
	}
	observed, err := d.Observe(ctx, process.SandboxID, process.Generation)
	if err != nil {
		return vmInfo{}, err
	}
	if observed.State != vmm.ProcessRunning || observed.Process != process {
		return vmInfo{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("VMM process changed during NIC operation"))
	}
	info, err := d.queryInfo(ctx, process.APISocket)
	if err != nil {
		return vmInfo{}, err
	}
	if info.State != "Running" {
		return vmInfo{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("VMM is %s, not Running", info.State))
	}
	return info, nil
}

// LiveNICs reports the TAP-to-device map for crash reconciliation.
func (d *Driver) LiveNICs(ctx context.Context, process vmm.Process) ([]vmm.NetworkDevice, error) {
	info, err := d.liveNetworkInfo(ctx, process)
	if err != nil {
		return nil, err
	}
	devices := make([]vmm.NetworkDevice, 0, len(info.Config.Nets))
	for _, net := range info.Config.Nets {
		if net.ID == "" || net.TAP == "" {
			return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("VMM reported NIC without ID or TAP"))
		}
		devices = append(devices, vmm.NetworkDevice{ID: net.ID, TAP: net.TAP, MAC: net.MAC})
	}
	return devices, nil
}

// AddNIC attaches one already prepared TAP. A stable device ID makes retries
// detectable through vm.info after a lost API acknowledgment.
func (d *Driver) AddNIC(ctx context.Context, process vmm.Process, device types.NetworkInterface) error {
	if err := device.Validate(); err != nil {
		return err
	}
	if _, err := d.liveNetworkInfo(ctx, process); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"id": fmt.Sprintf("kumabox-net-%d", device.Index), "tap": device.TAP, "mac": device.MAC,
		"num_queues": device.Queues, "queue_size": device.QueueSize,
		"offload_tso": true, "offload_ufo": true, "offload_csum": true,
	})
	if err != nil {
		return err
	}
	return d.snapshotAction(ctx, process.APISocket, "vm.add-net", payload, d.startupTimeout)
}

// RemoveNIC requests PCI eject and waits for the device to leave the VMM's
// device tree before the caller releases its TAP and CNI allocation.
func (d *Driver) RemoveNIC(ctx context.Context, process vmm.Process, id string) error {
	if id == "" {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("NIC device ID is empty"))
	}
	info, err := d.liveNetworkInfo(ctx, process)
	if err != nil {
		return err
	}
	if _, present := info.DeviceTree[id]; !present {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return err
	}
	requestErr := d.snapshotAction(ctx, process.APISocket, "vm.remove-device", payload, d.startupTimeout)
	deadline := time.NewTimer(nicEjectTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err = d.liveNetworkInfo(ctx, process)
		if err != nil {
			return errors.Join(requestErr, err)
		}
		if _, present := info.DeviceTree[id]; !present {
			return nil
		}
		if requestErr != nil {
			return requestErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("guest has not ejected NIC %s; bring it down inside the guest and retry", id))
		case <-ticker.C:
		}
	}
}
