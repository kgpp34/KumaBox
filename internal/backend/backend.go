// Package backend defines the VMM lifecycle boundary used by runtime.
//
// Runtime owns VM state transitions and cleanup policy. Backend implementations
// own rendering, starting, stopping, and observing the concrete VMM process.
package backend

import (
	"context"
	"io"
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vm"
)

// StateController exposes live VMM state transitions that do not create or
// terminate the backend process.
type StateController interface {
	PauseVM(context.Context, *vm.VMRecord) error
	ResumeVM(context.Context, *vm.VMRecord) error
}

// ConsoleController opens the live guest console stream for an interactive VM.
type ConsoleController interface {
	OpenConsole(context.Context, *vm.VMRecord) (io.ReadWriteCloser, error)
}

// DiskSpec identifies an externally owned raw disk to hot-plug.
type DiskSpec struct {
	Path     string
	Name     string
	ReadOnly bool
	DirectIO *bool
}

type AttachedDisk struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// DiskController is implemented by backends that support runtime virtio-blk
// hotplug. The backing file is never owned by the controller.
type DiskController interface {
	AttachDisk(context.Context, *vm.VMRecord, DiskSpec) (AttachedDisk, error)
	DetachDisk(context.Context, *vm.VMRecord, string) error
	ListDisks(context.Context, *vm.VMRecord) ([]AttachedDisk, error)
}

// NetworkController changes virtio-net devices on a running VM.
type NetworkController interface {
	AttachNetwork(context.Context, *vm.VMRecord, kbnetwork.Config) error
	DetachNetwork(context.Context, *vm.VMRecord, kbnetwork.Config) error
}

type FilesystemSpec struct {
	Socket, Tag          string
	NumQueues, QueueSize int
}
type AttachedFilesystem struct{ ID, Tag, Socket string }
type FilesystemController interface {
	AttachFilesystem(context.Context, *vm.VMRecord, FilesystemSpec) (AttachedFilesystem, error)
	DetachFilesystem(context.Context, *vm.VMRecord, string) error
	ListFilesystems(context.Context, *vm.VMRecord) ([]AttachedFilesystem, error)
}

type PCIDeviceSpec struct{ PCI, ID string }
type AttachedPCIDevice struct{ ID, PCI string }
type PCIDeviceController interface {
	AttachPCIDevice(context.Context, *vm.VMRecord, PCIDeviceSpec) (AttachedPCIDevice, error)
	DetachPCIDevice(context.Context, *vm.VMRecord, string) error
	ListPCIDevices(context.Context, *vm.VMRecord) ([]AttachedPCIDevice, error)
}

// DeviceState is the backend's live view of runtime-hotplugged devices.
type DeviceState struct {
	Disks       []AttachedDisk
	Filesystems []AttachedFilesystem
	PCIDevices  []AttachedPCIDevice
}

// DeviceInspector reads live device state without changing the VM.
type DeviceInspector interface {
	InspectDevices(context.Context, *vm.VMRecord) (DeviceState, error)
}

// NativeSnapshotter captures backend-owned memory, device, and VM state into
// an existing empty directory while the VM is paused.
type NativeSnapshotter interface {
	SnapshotVM(context.Context, *vm.VMRecord, string) error
}

// NativeRestorer recreates a backend process from validated native state.
// Runtime owns snapshot leases, writable disk replacement, and durable VM
// state transitions; implementations own backend-specific config patching and
// the restore/resume API sequence.
type NativeRestorer interface {
	RestoreVM(context.Context, *vm.VMRecord, string, string) (*StartResult, error)
}

// NativeCloner restores native state into a newly allocated VM identity and
// replaces snapshot network devices before vCPUs resume.
type NativeCloner interface {
	CloneVM(context.Context, *vm.VMRecord, string, string) (*StartResult, error)
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
	RestoreModes   []string
}

// NativeHostInspector reports the compatibility boundary for native backend
// state captured or restored on the current host.
type NativeHostInspector interface {
	InspectNativeHost(context.Context, *vm.VMRecord) (NativeHost, error)
}

// Lifecycle is the backend contract required by runtime.
//
// Implementations must make ObserveVM cheap and side-effect free because runtime
// calls it during inspect/list reconciliation.
type Lifecycle interface {
	RenderConfig(*vm.VMRecord) error
	StartVM(*vm.VMRecord) (*StartResult, error)
	StopVM(*vm.VMRecord, StopOptions) (*StopResult, error)
	ObserveVM(*vm.VMRecord) vm.Observation
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
