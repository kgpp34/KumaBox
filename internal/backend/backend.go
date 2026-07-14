// Package backend defines the VMM lifecycle boundary used by runtime.
//
// Runtime owns VM state transitions and cleanup policy. Backend implementations
// own rendering, starting, stopping, and observing the concrete VMM process.
package backend

import (
	"context"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

// StateController exposes live VMM state transitions that do not create or
// terminate the backend process.
type StateController interface {
	PauseVM(context.Context, *vmstore.VMRecord) error
	ResumeVM(context.Context, *vmstore.VMRecord) error
}

// NativeSnapshotter captures backend-owned memory, device, and VM state into
// an existing empty directory while the VM is paused.
type NativeSnapshotter interface {
	SnapshotVM(context.Context, *vmstore.VMRecord, string) error
}

// NativeRestorer recreates a backend process from validated native state.
// Runtime owns snapshot leases, writable disk replacement, and durable VM
// state transitions; implementations own backend-specific config patching and
// the restore/resume API sequence.
type NativeRestorer interface {
	RestoreVM(context.Context, *vmstore.VMRecord, string, string) (*StartResult, error)
}

// NativeHost describes host and backend properties that constrain whether a
// native snapshot can be restored safely.
type NativeHost struct {
	BackendName    string
	BackendVersion string
	SnapshotFormat string
	Architecture   string
	CPUVendor      string
	CPUFeatures    []string
}

// NativeHostInspector reports the compatibility boundary for native backend
// state captured or restored on the current host.
type NativeHostInspector interface {
	InspectNativeHost(context.Context, *vmstore.VMRecord) (NativeHost, error)
}

// Lifecycle is the backend contract required by runtime.
//
// Implementations must make ObserveVM cheap and side-effect free because runtime
// calls it during inspect/list reconciliation.
type Lifecycle interface {
	RenderConfig(*vmstore.VMRecord) error
	StartVM(*vmstore.VMRecord) (*StartResult, error)
	StopVM(*vmstore.VMRecord, StopOptions) (*StopResult, error)
	ObserveVM(*vmstore.VMRecord) vmstore.Observation
}

// StartResult contains process identity returned after a successful start.
type StartResult struct {
	PID       int
	APISocket string
}

// StopOptions controls graceful versus forced backend termination.
type StopOptions struct {
	Timeout time.Duration
	Force   bool
}

// StopResult is reserved for backend-specific stop details.
type StopResult struct{}
