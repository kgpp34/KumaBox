package core

import (
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
	"github.com/kumabox/kumabox/vmm/cloudhypervisor"
)

// vmmFactory assembles one concrete backend from application storage roots.
type vmmFactory func(storage.Roots) (vmm.Backend, error)

// vmmFactories is the single registration point for supported VMM adapters.
// Adding Firecracker means registering its constructor here; sandbox workflows
// continue to route through the persisted types.VMMType.
var vmmFactories = map[types.VMMType]vmmFactory{
	types.VMMCloudHypervisor: newCloudHypervisor,
}

// vmmBackends routes a persisted backend identity to its process adapter.
type vmmBackends map[types.VMMType]vmm.Backend

// openVMMBackends constructs every registered adapter so commands can operate
// on sandboxes created by different VMMs in the same metadata catalog.
func openVMMBackends(roots storage.Roots) (vmmBackends, error) {
	backends := make(vmmBackends, len(vmmFactories))
	for typ, factory := range vmmFactories {
		backend, err := factory(roots)
		if err != nil {
			return nil, fmt.Errorf("initialize VMM %s: %w", typ, err)
		}
		if backend == nil || backend.Type() != typ {
			return nil, fmt.Errorf("VMM factory %s returned a mismatched backend", typ)
		}
		backends[typ] = backend
	}
	return backends, nil
}

// backend returns the adapter that owns record. Unknown but syntactically
// valid identities are unsupported locally rather than treated as corruption.
func (b vmmBackends) backend(typ types.VMMType) (vmm.Backend, error) {
	if err := typ.Validate(); err != nil {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
	}
	backend, exists := b[typ]
	if !exists || backend == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q is not available", typ))
	}
	return backend, nil
}

// newCloudHypervisor owns the Cloud Hypervisor-specific dependency graph.
func newCloudHypervisor(roots storage.Roots) (vmm.Backend, error) {
	paths, err := vmm.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	scopes, err := cgroup.New("")
	if err != nil {
		return nil, err
	}
	backend, err := cloudhypervisor.New(paths, scopes, cloudhypervisor.Options{})
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("cloud-hypervisor constructor returned nil")
	}
	return backend, nil
}
