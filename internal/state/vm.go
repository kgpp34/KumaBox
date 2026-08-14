// Package state defines the persisted resource capabilities used by runtime
// orchestration. Implementations may use JSON files, SQLite, or another
// durable store without changing lifecycle code.
package state

import (
	"context"
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vm"
)

// VMReader contains read-only VM access. Callers that only inspect state
// should depend on this interface instead of the complete VM mutation API.
type VMReader interface {
	Inspect(string) (*vm.VMRecord, error)
	List() ([]*vm.VMRecord, error)
	RootDir() string
}

// VMEvents reports that persisted VM metadata may have changed. Notifications
// are hints: consumers must always reread VM records because events may be
// coalesced by the metadata backend.
type VMEvents interface {
	Events(context.Context) (<-chan struct{}, func(), error)
}

// VMRecords contains VM record creation and attachment mutations.
type VMRecords interface {
	Create(vm.CreateRequest) (*vm.VMRecord, error)
	Delete(string) error
	SetNetworkConfigs(string, []kbnetwork.Config) (*vm.VMRecord, error)
	SetAttachedDisks(string, []vm.AttachedDisk) (*vm.VMRecord, error)
	SetAttachedFilesystems(string, []vm.AttachedFilesystem) (*vm.VMRecord, error)
	SetAttachedPCIDevices(string, []vm.AttachedPCIDevice) (*vm.VMRecord, error)
}

// VMUpdater contains durable VM record updates. Ordinary state changes use
// UpdateStates; the remaining methods carry additional lifecycle data.
type VMUpdater interface {
	UpdateStates([]string, vm.VMState) error
	MarkStarted(string, int, string) (*vm.VMRecord, error)
	UpdatePerformance(string, vm.PerformanceMetrics) (*vm.VMRecord, error)
	CompleteHibernate(string, string) (*vm.VMRecord, error)
	SetError(string, string) (*vm.VMRecord, error)
}

// VMRestore contains durable markers for destructive and completed restores.
type VMRestore interface {
	BeginRestore(string, string, string) (*vm.VMRecord, error)
	FailRestore(string, string) (*vm.VMRecord, error)
	CompleteRestore(string, int, string, time.Duration, *vm.RestoreResult) (*vm.VMRecord, error)
}

// VMState is the complete VM resource state API consumed by the runtime.
//
// It is kept as a compatibility composition for existing constructors. New
// code should depend on the narrow capability it actually uses.
type VMState interface {
	VMReader
	VMRecords
	VMUpdater
	VMRestore
}

var _ VMState = (*vm.Store)(nil)
