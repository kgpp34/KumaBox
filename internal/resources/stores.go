// Package resources composes KumaBox's persisted resource stores.
package resources

import (
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// StoreSet is the complete persisted resource composition used by one
// KumaBox process. Store implementations can be replaced before handing the
// set to Runtime, GC, or a CLI command.
type StoreSet struct {
	VM        state.VMState
	Images    *imagestore.Store
	Snapshots *snapshot.Store
	Networks  *kbnetwork.Store
	OCI       *ocistore.Store
}

// NewStoreSet creates the default JSON-backed resource stores.
func NewStoreSet(rootDir string) StoreSet {
	return StoreSet{
		VM:        vmstore.New(rootDir),
		Images:    imagestore.New(rootDir),
		Snapshots: snapshot.NewStore(rootDir),
		Networks:  kbnetwork.NewStore(rootDir),
		OCI:       ocistore.New(rootDir),
	}
}
