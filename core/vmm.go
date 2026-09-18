package core

import (
	"fmt"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/vmm"
	"github.com/kumabox/kumabox/vmm/cloudhypervisor"
)

// openVMMRegistry assembles every backend enabled by this binary and freezes
// the routing table before a sandbox workflow can use it. Adding another VMM
// consists of constructing its adapter here and passing it to NewRegistry.
func openVMMRegistry(configuration config.Config) (*vmm.Registry, error) {
	paths, err := vmm.NewPaths(configuration.Paths)
	if err != nil {
		return nil, fmt.Errorf("initialize VMM paths: %w", err)
	}
	scopes, err := cgroup.New(configuration.VMM.CgroupParent)
	if err != nil {
		return nil, fmt.Errorf("initialize VMM cgroups: %w", err)
	}
	cloudHypervisor, err := cloudhypervisor.New(paths, scopes, cloudhypervisor.Options{
		Binary:         configuration.VMM.CloudHypervisor.Binary,
		StartupTimeout: configuration.VMM.CloudHypervisor.StartupTimeout,
		StopGrace:      configuration.VMM.CloudHypervisor.StopGrace,
		AbortGrace:     configuration.VMM.CloudHypervisor.AbortGrace,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize VMM cloud-hypervisor: %w", err)
	}
	registry, err := vmm.NewRegistry(cloudHypervisor)
	if err != nil {
		return nil, fmt.Errorf("initialize VMM registry: %w", err)
	}
	return registry, nil
}
