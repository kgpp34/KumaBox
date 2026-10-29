package types

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	validExternalDiskName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,19}$`)
	validPCIDeviceID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	validFileShareTag     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,35}$`)
)

const (
	// DefaultFileShareQueues is the vhost-user-fs queue count when unspecified.
	DefaultFileShareQueues = 1
	// DefaultFileShareQueueSize is the descriptor depth when unspecified.
	DefaultFileShareQueueSize = 1024
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

// FileShare is an externally served virtio-fs socket attached for one VMM run.
// KumaBox does not own the socket or the directory exported by its server.
type FileShare struct {
	Socket    string `json:"socket"`
	Tag       string `json:"tag"`
	NumQueues int    `json:"num_queues"`
	QueueSize int    `json:"queue_size"`
}

// NormalizeFileShare validates the portable mount tag and applies queue defaults.
func NormalizeFileShare(share FileShare) (FileShare, error) {
	if !filepath.IsAbs(share.Socket) {
		return share, fmt.Errorf("file share socket %q must be absolute", share.Socket)
	}
	if err := ValidateFileShareTag(share.Tag); err != nil {
		return share, err
	}
	if share.NumQueues < 0 {
		return share, fmt.Errorf("file share num-queues must not be negative")
	}
	if share.QueueSize < 0 {
		return share, fmt.Errorf("file share queue-size must not be negative")
	}
	if share.NumQueues == 0 {
		share.NumQueues = DefaultFileShareQueues
	}
	if share.QueueSize == 0 {
		share.QueueSize = DefaultFileShareQueueSize
	}
	return share, nil
}

// ValidateFileShareTag keeps the guest mount tag safe and VMM ID stable.
func ValidateFileShareTag(tag string) error {
	if !validFileShareTag.MatchString(tag) {
		return fmt.Errorf("file share tag %q must match %s", tag, validFileShareTag)
	}
	return nil
}

// AttachedFileShare is the live VMM view of a runtime-only virtio-fs device.
type AttachedFileShare struct {
	ID     string `json:"id"`
	Tag    string `json:"tag"`
	Socket string `json:"socket"`
}

// AttachedDevices contains runtime-only devices observed from the VMM. Empty
// slices mean no external devices are attached to the current process.
type AttachedDevices struct {
	Disks   []AttachedDisk      `json:"disks,omitempty"`
	FS      []AttachedFileShare `json:"fs,omitempty"`
	Devices []AttachedPCIDevice `json:"devices,omitempty"`
}
