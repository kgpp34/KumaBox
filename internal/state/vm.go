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

// VMLifecycle contains durable state changes around backend lifecycle events.
type VMLifecycle interface {
	MarkRunning(string, int, string) (*vmstore.VMRecord, error)
	MarkPerformance(string, vmstore.PerformanceMetrics) (*vmstore.VMRecord, error)
	MarkHibernated(string, string) (*vmstore.VMRecord, error)
	MarkPaused(string) (*vmstore.VMRecord, error)
	MarkResumed(string) (*vmstore.VMRecord, error)
	MarkError(string, string) (*vmstore.VMRecord, error)
	MarkStopped(string) (*vmstore.VMRecord, error)
}

// VMRestore contains durable markers for destructive and completed restores.
type VMRestore interface {
	BeginRestore(string, string, string) (*vmstore.VMRecord, error)
	MarkRestoreFailed(string, string) (*vmstore.VMRecord, error)
	MarkRestored(string, int, string, time.Duration) (*vmstore.VMRecord, error)
	MarkRestoredWithMetrics(string, int, string, time.Duration, *vmstore.RestoreResult) (*vmstore.VMRecord, error)
}

// VMState is the complete VM resource state API consumed by the runtime.
//
// It is kept as a compatibility composition for existing constructors. New
// code should depend on the narrow capability it actually uses.
type VMState interface {
	VMReader
	VMRecords
	VMLifecycle
	VMRestore
}

var _ VMState = (*vmstore.Store)(nil)
