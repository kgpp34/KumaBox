// SPDX-License-Identifier: MIT

package imagestore

import "time"

// Source describes where a managed image was imported from.
type Source struct {
	Type string `json:"type"`
	URI  string `json:"uri,omitempty"`
}

// RootDisk describes the managed root disk stored with an image.
type RootDisk struct {
	Path             string `json:"path,omitempty"`
	Format           string `json:"format,omitempty"`
	VirtualSizeBytes int64  `json:"virtualSizeBytes,omitempty"`
	ActualSizeBytes  int64  `json:"actualSizeBytes,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
}

// Boot describes how VMs should boot from an image.
type Boot struct {
	Mode     string `json:"mode,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Initrd   string `json:"initrd,omitempty"`
	Cmdline  string `json:"cmdline,omitempty"`
}

// OS describes the guest operating system profile for an image.
type OS struct {
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// ImageRecord is the persisted metadata for one managed image.
type ImageRecord struct {
	SchemaVersion string    `json:"schemaVersion"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Source        Source    `json:"source"`
	RootDisk      RootDisk  `json:"rootDisk"`
	Boot          Boot      `json:"boot"`
	OS            OS        `json:"os"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func cloneRecord(rec *ImageRecord) *ImageRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	return &copied
}
