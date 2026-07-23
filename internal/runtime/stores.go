package runtime

import (
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// StoreSet is the runtime's resource-store composition. Keeping these
// instances together prevents lifecycle code from silently creating a second
// store with a different metadata engine.
type StoreSet struct {
	VM        state.VMState
	Images    *imagestore.Store
	Snapshots *snapshot.Store
	Networks  *kbnetwork.Store
	OCI       *ocistore.Store
}

func newStoreSet(rootDir string, vm state.VMState) StoreSet {
	return StoreSet{
		VM:        vm,
		Images:    imagestore.New(rootDir),
		Snapshots: snapshot.NewStore(rootDir),
		Networks:  kbnetwork.NewStore(rootDir),
		OCI:       ocistore.New(rootDir),
	}
}

// NewStoreSet creates the default JSON-backed resource stores. The returned
// set is intentionally concrete so callers can replace individual stores with
// injected-engine variants before constructing a Runtime.
func NewStoreSet(rootDir string) StoreSet {
	return newStoreSet(rootDir, vmstore.New(rootDir))
}
