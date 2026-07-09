// SPDX-License-Identifier: MIT

// Package imagestore manages imported and pulled cloud images.
//
// Image records are metadata only: they point at managed root disks and boot
// requirements. VM records copy the resolved image reference at create/run time
// so later image renames do not change existing VM intent.
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

// OCIPlatform identifies the image platform selected during OCI resolution.
type OCIPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// OCIDescriptor records one digest-addressed OCI object.
type OCIDescriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// EROFSLayer records the converted read-only filesystem for one OCI layer.
type EROFSLayer struct {
	Path        string `json:"path"`
	Filesystem  string `json:"filesystem"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"sizeBytes"`
	SourceLayer string `json:"sourceLayer"`
}

// OCILayer records one OCI layer and its converted shared filesystem.
type OCILayer struct {
	Index     int         `json:"index"`
	Digest    string      `json:"digest"`
	MediaType string      `json:"mediaType"`
	SizeBytes int64       `json:"sizeBytes"`
	EROFS     *EROFSLayer `json:"erofs,omitempty"`
}

// OCI records the OCI source and layer order for an image build.
type OCI struct {
	Ref       string        `json:"ref"`
	Source    string        `json:"source"`
	DigestRef string        `json:"digestRef"`
	Platform  OCIPlatform   `json:"platform"`
	Config    OCIDescriptor `json:"config"`
	Layers    []OCILayer    `json:"layers"`
	BuiltAt   time.Time     `json:"builtAt"`
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
	OCI           *OCI      `json:"oci,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func cloneRecord(rec *ImageRecord) *ImageRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	if rec.OCI != nil {
		oci := *rec.OCI
		oci.Layers = append([]OCILayer(nil), rec.OCI.Layers...)
		for i := range oci.Layers {
			if oci.Layers[i].EROFS == nil {
				continue
			}
			erofs := *oci.Layers[i].EROFS
			oci.Layers[i].EROFS = &erofs
		}
		copied.OCI = &oci
	}
	return &copied
}
