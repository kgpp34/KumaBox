package runtime

import (
	"github.com/kumabox/kumabox/internal/resources"
	"github.com/kumabox/kumabox/internal/state"
)

// StoreSet is kept as a runtime alias for source compatibility. The actual
// resource composition belongs to the resources package so other entrypoints
// can use the same construction boundary.
type StoreSet = resources.StoreSet

func newStoreSet(rootDir string, vm state.VMState) StoreSet {
	stores := resources.NewStoreSet(rootDir)
	stores.VM = vm
	return stores
}

// NewStoreSet creates the default resource composition for runtime callers.
func NewStoreSet(rootDir string) StoreSet {
	return resources.NewStoreSet(rootDir)
}
