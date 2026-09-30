package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// Backend is the stable process-level contract implemented by every VMM
// adapter. Durable sandbox transitions remain in core; implementations own
// process launch, identity, control APIs, console access, and runtime cleanup.
type Backend interface {
	Type() types.VMMType
	Preflight() error
	Locate(context.Context, types.SandboxID, uint64) (Process, bool, error)
	Observe(context.Context, types.SandboxID, uint64) (Observation, error)
	WaitReady(context.Context, Process) error
	Launch(context.Context, LaunchPlan) (Process, error)
	Abort(context.Context, Process) error
	Stop(context.Context, Process) error
	Console(context.Context, Process) (io.ReadWriteCloser, error)
	DialVsock(context.Context, Process, uint32) (io.ReadWriteCloser, error)
	Logs(context.Context, types.SandboxID, LogOptions, io.Writer) error
	Cleanup(context.Context, types.SandboxID) error
	RemoveLogs(context.Context, types.SandboxID) error
}

// NetworkDevice is a NIC currently reported by a running VMM. TAP identity
// lets the service reconcile interrupted hotplug against the CNI journal.
type NetworkDevice struct {
	ID  string
	TAP string
	MAC string
}

// NetworkHotplugger is an optional VMM capability for one-at-a-time live NIC
// changes. The service owns CNI allocation and durable sandbox state.
type NetworkHotplugger interface {
	LiveNICs(context.Context, Process) ([]NetworkDevice, error)
	AddNIC(context.Context, Process, types.NetworkInterface) error
	RemoveNIC(context.Context, Process, string) error
}

// DiskHotplugger manages external raw disks for one VMM run. Its methods never
// create or delete the backing file; callers serialize them with capture/stop.
type DiskHotplugger interface {
	AttachedDisks(context.Context, Process) ([]types.AttachedDisk, error)
	AddDisk(context.Context, Process, types.ExternalDisk) error
	RemoveDisk(context.Context, Process, string) error
}

// PCIHotplugger manages VFIO passthrough devices for one VMM run. Host binding
// and IOMMU configuration remain administrator responsibilities.
type PCIHotplugger interface {
	AttachedPCIDevices(context.Context, Process) ([]types.AttachedPCIDevice, error)
	AddPCIDevice(context.Context, Process, types.PCIDevice) error
	RemovePCIDevice(context.Context, Process, string) error
}

// SnapshotFile describes one writable disk copied inside the VMM pause window.
type SnapshotFile struct {
	// Source is the current sandbox-owned writable disk.
	Source string
	// Destination is an absent path inside the private capture directory.
	Destination string
}

// SnapshotPlan contains all inputs required for one consistent live capture.
type SnapshotPlan struct {
	// Process is the exact VMM generation being captured.
	Process Process
	// Destination receives native VMM memory and device-state files.
	Destination string
	// WritableFiles are copied while the guest remains paused.
	WritableFiles []SnapshotFile
}

// Snapshotter is the optional live-capture capability implemented by VMMs that
// can pause, save native state, copy writable disks, and resume safely.
type Snapshotter interface {
	Snapshot(context.Context, SnapshotPlan) error
}

// Hibernator captures one paused VM, calls persist before it can run again,
// then terminates the exact process. A persist failure resumes the VM.
// Implementations must not resume after termination has been attempted.
type Hibernator interface {
	Hibernate(context.Context, SnapshotPlan, func() error) error
}

// RestorePlan contains the immutable ownership and native capture inputs for a
// VMM restore launch.
type RestorePlan struct {
	// SandboxID owns the restored process and runtime files.
	SandboxID types.SandboxID
	// Generation is the durable Starting generation for this launch.
	Generation uint64
	// CPUs sizes the process cgroup consistently with a normal launch.
	CPUs uint32
	// SnapshotDir contains native VMM state with already restored writable disks.
	SnapshotDir string
	// Network supplies the recovered namespace and stable TAP identities.
	Network types.NetworkSetup
}

// Restorer is the optional native-state restore capability implemented by VMMs
// whose snapshot format can resume a stopped process.
type Restorer interface {
	Restore(context.Context, RestorePlan) (Process, error)
}

// ClonePlan owns a new sandbox restored from an immutable snapshot. The VMM
// adapter copies native state and replaces source-specific device bindings.
type ClonePlan struct {
	RestorePlan
	// WritableDisk is the new sandbox's COW path already populated from the capture.
	WritableDisk string
	// ImageDisks are read-only layers in manifest order at the target data root.
	ImageDisks []Disk
	// DataDisks are target-owned copies of the captured writable disks.
	DataDisks []Disk
	// NewDataDisks are private blank disks hot-added before clone resume.
	NewDataDisks []Disk
	// Kernel and Initrd are the target root's selected boot artifacts.
	Kernel string
	Initrd string
}

// Cloner is the optional native-state clone capability. It must never mutate
// SnapshotDir or attach the source sandbox's writable disk and network devices.
type Cloner interface {
	Clone(context.Context, ClonePlan) (Process, error)
}

// RestoreValidator optionally validates native snapshot files before a running
// sandbox is stopped for restore.
type RestoreValidator interface {
	ValidateRestore(context.Context, string) error
}

// Registry is an immutable routing table from durable VMM identities to their
// process adapters. Construction validates the complete backend set so runtime
// lookup cannot depend on package initialization or registration order.
type Registry struct {
	backends map[types.VMMType]Backend
}

// NewRegistry validates and freezes the supplied backend set.
func NewRegistry(backends ...Backend) (*Registry, error) {
	registered := make(map[types.VMMType]Backend, len(backends))
	for _, backend := range backends {
		if backend == nil || isNilBackend(backend) {
			return nil, errors.New("VMM registry contains a nil backend")
		}
		typ := backend.Type()
		if err := typ.Validate(); err != nil {
			return nil, fmt.Errorf("register VMM backend: %w", err)
		}
		if _, exists := registered[typ]; exists {
			return nil, fmt.Errorf("VMM backend %q is registered more than once", typ)
		}
		registered[typ] = backend
	}
	return &Registry{backends: registered}, nil
}

// isNilBackend catches typed nil pointers stored inside a non-nil interface.
func isNilBackend(backend Backend) bool {
	value := reflect.ValueOf(backend)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Backend returns the adapter for a persisted VMM identity. An unknown valid
// type means this installation lacks the required adapter; an invalid type is
// treated as corrupt durable state.
func (r *Registry) Backend(typ types.VMMType) (Backend, error) {
	if err := typ.Validate(); err != nil {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
	}
	if r == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("VMM registry is not configured"))
	}
	backend, exists := r.backends[typ]
	if !exists || backend == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q is not available", typ))
	}
	return backend, nil
}

// Len returns the number of backends frozen into the registry.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.backends)
}

// Backends returns the configured adapters in stable identity order for
// cross-backend maintenance. The registry itself remains immutable.
func (r *Registry) Backends() []Backend {
	if r == nil {
		return nil
	}
	identities := make([]types.VMMType, 0, len(r.backends))
	for identity := range r.backends {
		identities = append(identities, identity)
	}
	slices.Sort(identities)
	backends := make([]Backend, 0, len(identities))
	for _, identity := range identities {
		backends = append(backends, r.backends[identity])
	}
	return backends
}
