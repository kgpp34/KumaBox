package snapshot

import "time"

// Manifest is the portable description stored beside snapshot disk payloads.
type Manifest struct {
	SchemaVersion string           `json:"schemaVersion"`
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Type          string           `json:"type"`
	Consistency   string           `json:"consistency"`
	Source        Source           `json:"source"`
	Base          *Base            `json:"base,omitempty"`
	Disks         []DiskManifest   `json:"disks"`
	Native        *NativeManifest  `json:"native,omitempty"`
	Backend       *BackendManifest `json:"backend,omitempty"`
	Machine       *MachineManifest `json:"machine,omitempty"`
	Boot          *BootManifest    `json:"boot,omitempty"`
	Devices       *DeviceManifest  `json:"devices,omitempty"`
	Network       *NetworkManifest `json:"network,omitempty"`
	CreatedAt     time.Time        `json:"createdAt"`
}

// NativeManifest inventories backend-owned running snapshot payload.
type NativeManifest struct {
	PayloadDir string               `json:"payloadDir"`
	Files      []NativeFileManifest `json:"files"`
}

type NativeFileManifest struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type BackendManifest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	SnapshotFormat string `json:"snapshotFormat"`
}

type MachineManifest struct {
	Architecture string   `json:"architecture"`
	CPUVendor    string   `json:"cpuVendor"`
	CPUFeatures  []string `json:"cpuFeatures,omitempty"`
	VCPUs        int      `json:"vcpus"`
	MemoryBytes  int64    `json:"memoryBytes"`
}

type BootManifest struct {
	Mode           string `json:"mode"`
	KernelDigest   string `json:"kernelDigest,omitempty"`
	InitrdDigest   string `json:"initrdDigest,omitempty"`
	FirmwareDigest string `json:"firmwareDigest,omitempty"`
}

type DeviceManifest struct {
	Disks []StorageDeviceManifest `json:"disks"`
	NICs  int                     `json:"nics"`
	Vsock bool                    `json:"vsock"`
}

type StorageDeviceManifest struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
	Format   string `json:"format"`
}

type NetworkManifest struct {
	RestorePolicy string `json:"restorePolicy"`
	ClonePolicy   string `json:"clonePolicy"`
}

type Source struct {
	VMID        string `json:"vmId"`
	VMName      string `json:"vmName"`
	ImageID     string `json:"imageId,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
}

type Base struct {
	Family       string   `json:"family"`
	ImageID      string   `json:"imageId"`
	Digest       string   `json:"digest"`
	Format       string   `json:"format,omitempty"`
	LayerDigests []string `json:"layerDigests,omitempty"`
}

type DiskManifest struct {
	ID                 string `json:"id"`
	Role               string `json:"role"`
	Path               string `json:"path"`
	Format             string `json:"format"`
	Filesystem         string `json:"filesystem,omitempty"`
	VirtualSizeBytes   int64  `json:"virtualSizeBytes"`
	AllocatedSizeBytes int64  `json:"allocatedSizeBytes"`
	SHA256             string `json:"sha256"`
	CopyStrategy       string `json:"copyStrategy"`
}
