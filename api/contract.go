// Package api defines the versioned HTTP contract used by remote KumaBox clients.
// Its JSON shapes are independent of core's persistence and VMM types.
package api

import (
	"time"

	"github.com/kumabox/kumabox/types"
)

// CreateSandboxRequest is the client-controlled resource shape for one VM.
// Omitted numeric fields use the same defaults as the local CLI.
type CreateSandboxRequest struct {
	Image        string     `json:"image"`
	Name         string     `json:"name"`
	CPUs         *uint32    `json:"cpus,omitempty"`
	Memory       *int64     `json:"memory,omitempty"`
	Storage      *int64     `json:"storage,omitempty"`
	NICs         *int       `json:"nics,omitempty"`
	Network      string     `json:"network,omitempty"`
	SharedMemory bool       `json:"shared_memory,omitempty"`
	DataDisks    []DataDisk `json:"data_disks,omitempty"`
	Start        *bool      `json:"start,omitempty"`
}

// CreateSandboxInput is the application-neutral call passed to a sandbox service.
type CreateSandboxInput struct {
	ImageReference string
	Config         types.SandboxConfig
}

// SaveSnapshotInput is the application-neutral call passed to a snapshot service.
type SaveSnapshotInput struct {
	SandboxReference string
	Name             string
	Description      string
}

// DataDisk is one sandbox-owned volume in an API request or response.
type DataDisk struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	FSType   string `json:"fstype"`
	DirectIO *bool  `json:"direct_io,omitempty"`
}

// Sandbox is the stable public projection of a durable sandbox record.
type Sandbox struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	ImageDigest  string     `json:"image_digest"`
	VMM          string     `json:"vmm"`
	State        string     `json:"state"`
	CPUs         uint32     `json:"cpus"`
	Memory       int64      `json:"memory"`
	Storage      int64      `json:"storage"`
	NICs         int        `json:"nics"`
	Network      string     `json:"network,omitempty"`
	SharedMemory bool       `json:"shared_memory"`
	DataDisks    []DataDisk `json:"data_disks,omitempty"`
	Generation   uint64     `json:"generation"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Snapshot is the stable public projection of a captured sandbox state.
type Snapshot struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	SandboxID   string    `json:"sandbox_id"`
	ImageDigest string    `json:"image_digest"`
	VMM         string    `json:"vmm"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// Error is a machine-readable failure, including partial-commit information.
type Error struct {
	// Code is stable for client-side branching.
	Code string `json:"code"`
	// Message is a diagnostic and may change between releases.
	Message string `json:"message"`
	// Committed means the operation changed durable state despite returning an error.
	Committed bool `json:"committed"`
	// ResourceID identifies a retained sandbox after a partially committed create.
	ResourceID string `json:"resource_id,omitempty"`
}

// ErrorResponse wraps failures consistently across unary and stream endpoints.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// ExecRequest runs an argv without a shell; stdin is base64-encoded bytes.
type ExecRequest struct {
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env,omitempty"`
	Stdin string            `json:"stdin,omitempty"`
}

// ExecEvent is one NDJSON output frame. Data is base64 for lossless byte streams.
// The terminal event is either an exit_code or an error.
type ExecEvent struct {
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    *Error `json:"error,omitempty"`
}

// SaveSnapshotRequest captures a running sandbox with optional operator labels.
type SaveSnapshotRequest struct {
	Sandbox     string `json:"sandbox"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// CloneRequest creates a new sandbox from one ready snapshot.
type CloneRequest struct {
	Name string `json:"name"`
}

// RestoreRequest selects one snapshot for in-place restore.
type RestoreRequest struct {
	Snapshot string `json:"snapshot"`
}

func sandboxRecord(record types.Sandbox) Sandbox {
	disks := make([]DataDisk, 0, len(record.Config.DataDisks))
	for _, disk := range record.Config.DataDisks {
		disks = append(disks, DataDisk{Name: disk.Name, Size: disk.Size, FSType: disk.FSType, DirectIO: disk.DirectIO})
	}
	return Sandbox{
		ID: record.ID.String(), Name: record.Config.Name, ImageDigest: record.ImageDigest.String(),
		VMM: string(record.VMM), State: string(record.State), CPUs: record.Config.CPUs,
		Memory: record.Config.Memory, Storage: record.Config.Storage, NICs: record.Config.NICs,
		Network: record.Config.NetworkName, SharedMemory: record.Config.SharedMemory,
		DataDisks: disks, Generation: record.Generation, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func snapshotRecord(record types.Snapshot) Snapshot {
	return Snapshot{
		ID: record.ID.String(), Name: record.Name, Description: record.Description,
		SandboxID: record.SandboxID.String(), ImageDigest: record.ImageDigest.String(),
		VMM: string(record.VMM), Size: record.Size, CreatedAt: record.CreatedAt,
	}
}
