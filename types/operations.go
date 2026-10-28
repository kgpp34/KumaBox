package types

import "time"

// RuntimeState describes a verified VMM process observation.
type RuntimeState string

// CreateSandboxRequest contains image selection and the immutable guest resource shape.
type CreateSandboxRequest struct {
	// ImageReference is an existing local image alias or manifest digest.
	ImageReference string
	// Config contains the immutable name and guest resource shape.
	Config SandboxConfig
	// VMM selects the runtime backend; empty uses the configured default.
	VMM VMMType
}

// SaveSnapshotRequest labels a capture of one running sandbox.
type SaveSnapshotRequest struct {
	// SandboxReference is the source sandbox name or complete ID.
	SandboxReference string
	// Name is an optional unique snapshot lookup key.
	Name string
	// Description is optional operator context stored with the snapshot.
	Description string
}

// RestoreOptions selects the capture source and native memory loading policy.
type RestoreOptions struct {
	// MemoryMode overrides automatic native memory restoration for this launch.
	MemoryMode MemoryRestoreMode
	// SourceDirectory selects a portable capture instead of a catalog snapshot.
	SourceDirectory string
	// Force accepts a different source sandbox identity if the shape matches.
	Force bool
	// Pull fetches a missing registry image by the capture's exact digest.
	Pull bool
}

// CloneOptions selects a snapshot source and the new sandbox identity.
type CloneOptions struct {
	// Name is the required identity assigned to the new sandbox.
	Name string
	// CPUPolicy replaces source-host scheduling with the clone request.
	CPUPolicy CPUPolicy
	// Pull fetches a missing registry image by the snapshot's exact digest.
	Pull bool
	// SourceDirectory selects a portable directory instead of a catalog snapshot.
	SourceDirectory string
	// MemoryMode overrides automatic native memory restoration for this clone.
	MemoryMode MemoryRestoreMode
	// NICs overrides the captured interface count, including zero.
	NICs *int
	// NetworkName selects another CNI network for the new interfaces.
	NetworkName string
	// DataDisks are new disks attached to the clone after snapshot restoration.
	DataDisks []DataDiskSpec
	// WaitForNetwork applies the clone's new guest identity and NIC settings
	// before returning. By default, clone returns after VMM resume, matching
	// the explicit post-clone guest configuration workflow.
	WaitForNetwork bool
	// SkipReseed delegates post-resume entropy and machine-ID renewal to the
	// caller. The node service ignores this escape hatch and always reseeds.
	SkipReseed bool
}

// SandboxStatus combines durable state with a verified runtime observation.
type SandboxStatus struct {
	// Sandbox is the current durable record.
	Sandbox Sandbox
	// Runtime is absent, starting or running after identity verification.
	Runtime RuntimeState
	// PID is the observed host process, not a guest process ID.
	PID int
	// Stale reports a Running record whose VMM no longer exists.
	Stale bool
}

// SandboxLogOptions selects the retained VMM output to stream.
type SandboxLogOptions struct {
	// Tail starts output at the last N lines. Zero selects the complete log.
	Tail int
	// Follow keeps the stream open for appended output until cancellation.
	Follow bool
}

// GCAction identifies a completed lifecycle repair or orphan cleanup.
type GCAction struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Action string `json:"action"`
}

// GCReport retains successful cleanup actions alongside skipped resources.
type GCReport struct {
	Actions []GCAction `json:"actions"`
	Skipped int        `json:"skipped"`
}

// SnapshotEvictionPolicy controls opt-in eviction of ready snapshots.
type SnapshotEvictionPolicy struct {
	// Enabled opts into evicting ready snapshots; zero limits select all.
	Enabled bool
	// DryRun previews eviction without deleting selected snapshots.
	DryRun bool
	// KeepLast retains this many most recently used snapshots.
	KeepLast int
	// MaxAge evicts snapshots not used within this duration.
	MaxAge time.Duration
	// MaxSize evicts least recently used snapshots until total size fits.
	MaxSize int64
}

// NodeInfo identifies the running server and its authoritative ownership roots.
// Clients use these paths for API adapter state rather than guessing local policy.
type NodeInfo struct {
	// Version is the server build identifier.
	Version string
	// DataRoot owns persistent images, sandbox records and snapshot artifacts.
	DataRoot string
	// RunRoot owns process identities, API sockets and operation locks.
	RunRoot string
	// LogRoot owns retained VMM and runtime output.
	LogRoot string
}
