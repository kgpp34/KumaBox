package types

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	validExternalDiskName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)
	validPCIDeviceID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// ExternalDisk is an existing host raw file attached for one VMM run. Its
// backing file is never owned, snapshotted, or removed by KumaBox.
type ExternalDisk struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"readonly,omitempty"`
	DirectIO bool   `json:"direct_io"`
	// Queues matches the sandbox CPU count and is not part of the user input.
	Queues uint32 `json:"-"`
}

// ValidateExternalDiskName keeps the guest serial and VMM device ID stable.
func ValidateExternalDiskName(name string) error {
	if !validExternalDiskName.MatchString(name) || strings.HasPrefix(name, "kumabox-") {
		return fmt.Errorf("disk name %q must match %s and not start with kumabox-", name, validExternalDiskName)
	}
	return nil
}

// AttachedDisk is the live VMM view of a runtime-only external disk.
type AttachedDisk struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// PCIDevice is one VFIO host device requested for this VMM run.
type PCIDevice struct {
	// Path is a canonical /sys/bus/pci/devices/<BDF> path.
	Path string
	// ID is the explicit detach key reported by the VMM.
	ID string
}

// ValidatePCIDeviceID rejects IDs that could collide with KumaBox devices.
func ValidatePCIDeviceID(id string) error {
	if !validPCIDeviceID.MatchString(id) || strings.HasPrefix(id, "kumabox-") {
		return fmt.Errorf("device id %q must match %s and not start with kumabox-", id, validPCIDeviceID)
	}
	return nil
}

// AttachedPCIDevice is the live VMM view of a passed-through PCI device.
type AttachedPCIDevice struct {
	ID  string `json:"id"`
	BDF string `json:"bdf"`
}

// AttachedDevices contains runtime-only devices observed from the VMM. Empty
// slices mean no external devices are attached to the current process.
type AttachedDevices struct {
	Disks   []AttachedDisk      `json:"disks,omitempty"`
	Devices []AttachedPCIDevice `json:"devices,omitempty"`
}
