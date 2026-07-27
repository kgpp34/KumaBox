package cloudhypervisor

import (
	"context"
	"errors"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// InspectDevices obtains one vm.info snapshot and derives all KumaBox-owned
// hotplug devices from it.
func (b Backend) InspectDevices(ctx context.Context, rec *vmstore.VMRecord) (backend.DeviceState, error) {
	if rec == nil {
		return backend.DeviceState{}, errors.New("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return backend.DeviceState{}, err
	}
	state := backend.DeviceState{}
	for _, disk := range info.Config.Disks {
		if strings.HasPrefix(disk.ID, diskIDPrefix) {
			name := strings.TrimPrefix(disk.ID, diskIDPrefix)
			if validDiskName(name) {
				state.Disks = append(state.Disks, backend.AttachedDisk{ID: disk.ID, Name: name, Path: disk.Path, ReadOnly: disk.ReadOnly})
			}
		}
	}
	for _, fs := range info.Config.Fs {
		if strings.HasPrefix(fs.ID, filesystemIDPrefix) {
			state.Filesystems = append(state.Filesystems, backend.AttachedFilesystem{ID: fs.ID, Tag: fs.Tag, Socket: fs.Socket})
		}
	}
	for _, device := range info.Config.Devices {
		if strings.HasPrefix(device.ID, "kumabox-pci-") {
			state.PCIDevices = append(state.PCIDevices, backend.AttachedPCIDevice{ID: device.ID, PCI: device.Path})
		}
	}
	return state, nil
}
