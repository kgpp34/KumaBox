package snapshot

import "time"

// Manifest is the portable description stored beside snapshot disk payloads.
type Manifest struct {
	SchemaVersion string          `json:"schemaVersion"`
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Consistency   string          `json:"consistency"`
	Source        Source          `json:"source"`
	Base          *Base           `json:"base,omitempty"`
	Disks         []DiskManifest  `json:"disks"`
	Native        *NativeManifest `json:"native,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// NativeManifest inventories backend-owned running snapshot payload.
type NativeManifest struct {
	PayloadDir string               `json:"payloadDir"`
	Files      []NativeFileManifest `json:"files"`
}

type NativeFileManifest struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
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
