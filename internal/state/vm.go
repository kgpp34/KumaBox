// Package state defines the persisted resource capabilities used by runtime
// orchestration. Implementations may use JSON files, SQLite, or another
// durable store without changing lifecycle code.
package state

import (
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// VMReader contains read-only VM access. Callers that only inspect state
// should depend on this interface instead of the complete VM mutation API.
type VMReader interface {
	Inspect(string) (*vmstore.VMRecord, error)
	List() ([]*vmstore.VMRecord, error)
	RootDir() string
}

// VMRecords contains VM record creation and attachment mutations.
type VMRecords interface {
	Create(vmstore.CreateRequest) (*vmstore.VMRecord, error)
	Delete(string) error
	SetNetworkConfigs(string, []kbnetwork.Config) (*vmstore.VMRecord, error)
}

// VMUpdater contains durable VM record updates. Ordinary state changes use
// UpdateStates; the remaining methods carry additional lifecycle data.
type VMUpdater interface {
	UpdateStates([]string, vmstore.VMState) error
	MarkStarted(string, int, string) (*vmstore.VMRecord, error)
	UpdatePerformance(string, vmstore.PerformanceMetrics) (*vmstore.VMRecord, error)
	CompleteHibernate(string, string) (*vmstore.VMRecord, error)
	SetError(string, string) (*vmstore.VMRecord, error)
}

// VMRestore contains durable markers for destructive and completed restores.
type VMRestore interface {
	BeginRestore(string, string, string) (*vmstore.VMRecord, error)
	FailRestore(string, string) (*vmstore.VMRecord, error)
	CompleteRestore(string, int, string, time.Duration, *vmstore.RestoreResult) (*vmstore.VMRecord, error)
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

var _ VMState = (*vmstore.Store)(nil)
