// Package state defines the persisted resource capabilities used by runtime
// orchestration. Implementations may use JSON files, SQLite, or another
// durable store without changing lifecycle code.
package state

import (
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// VMState is the VM resource state API consumed by the runtime.
//
// The interface deliberately exposes lifecycle transitions rather than the
// storage format. Runtime owns side effects such as VMM and network changes;
// this API only publishes the durable VM state around those boundaries.
type VMState interface {
	Create(vmstore.CreateRequest) (*vmstore.VMRecord, error)
	Inspect(string) (*vmstore.VMRecord, error)
	Delete(string) error
	List() ([]*vmstore.VMRecord, error)
	RootDir() string

	MarkRunning(string, int, string) (*vmstore.VMRecord, error)
	MarkPerformance(string, vmstore.PerformanceMetrics) (*vmstore.VMRecord, error)
	BeginRestore(string, string, string) (*vmstore.VMRecord, error)
	MarkRestoreFailed(string, string) (*vmstore.VMRecord, error)
	MarkRestored(string, int, string, time.Duration) (*vmstore.VMRecord, error)
	MarkRestoredWithMetrics(string, int, string, time.Duration, *vmstore.RestoreResult) (*vmstore.VMRecord, error)
	MarkHibernated(string, string) (*vmstore.VMRecord, error)
	MarkPaused(string) (*vmstore.VMRecord, error)
	MarkResumed(string) (*vmstore.VMRecord, error)
	MarkError(string, string) (*vmstore.VMRecord, error)
	MarkStopped(string) (*vmstore.VMRecord, error)
	SetNetworkConfigs(string, []kbnetwork.Config) (*vmstore.VMRecord, error)
}

var _ VMState = (*vmstore.Store)(nil)
