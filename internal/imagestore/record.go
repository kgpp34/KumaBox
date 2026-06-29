package imagestore

import "time"

type Source struct {
	Type string `json:"type"`
	URI  string `json:"uri,omitempty"`
}

type RootDisk struct {
	Path             string `json:"path,omitempty"`
	Format           string `json:"format,omitempty"`
	VirtualSizeBytes int64  `json:"virtualSizeBytes,omitempty"`
	ActualSizeBytes  int64  `json:"actualSizeBytes,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
}

type Boot struct {
	Mode     string `json:"mode,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Initrd   string `json:"initrd,omitempty"`
	Cmdline  string `json:"cmdline,omitempty"`
}

type OS struct {
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
	Profile string `json:"profile,omitempty"`
}

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
