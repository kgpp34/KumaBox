package core

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// NetResize changes a running sandbox's NIC count one slot at a time. The
// operation lock spans CNI, VMM, guest and metadata work; retries first sweep
// allocations beyond the durable count left by an interrupted addition.
//
//	grow:   CNI ADD -> VMM add -> guest config -> metadata
//	shrink: guest config -> VMM eject -> CNI DEL -> metadata
func (s *SandboxService) NetResize(ctx context.Context, reference string, target int) (result types.Sandbox, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.networks == nil || s.dependencies.runtimes.Len() == 0 {
		return types.Sandbox{}, errors.New("sandbox service is not configured")
	}
	if reference == "" || target < 0 || target > types.MaxSandboxNICs {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("SANDBOX and --nics between 0 and %d are required", types.MaxSandboxNICs))
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, errdefs.Context(err, "resize sandbox network", reference, "lock", "retry the resize", false)
	}
	defer func() {
		if err := lock.Unlock(context.WithoutCancel(ctx)); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	if record.State != types.SandboxStateRunning || record.Generation < 2 {
		return record, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not Running", record.ID, record.State))
	}
	if record.Network.Backend == "" {
		return record, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("this sandbox has no private network namespace; create a new sandbox to enable live NIC resize"))
	}
	provider, err := s.dependencies.networks.Provider(record.Network.Backend)
	if err != nil {
		return record, err
	}
	resizer, ok := provider.(network.Resizer)
	if !ok {
		return record, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("network backend %s cannot resize live NICs", record.Network.Backend))
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return record, err
	}
	hotplugger, ok := backend.(vmm.NetworkHotplugger)
	if !ok {
		return record, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM %s cannot resize live NICs", record.VMM))
	}
	process, exists, err := backend.Locate(ctx, record.ID, record.Generation-1)
	if err != nil {
		return record, err
	}
	if !exists {
		return record, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("running sandbox VMM is absent"))
	}
	if err := reconcileNICs(ctx, record, target, resizer, hotplugger, process); err != nil {
		return record, errdefs.Context(err, "resize sandbox network", reference, "reconcile previous operation", "inspect the sandbox and retry", false)
	}
	initialNICs := record.Config.NICs
	for record.Config.NICs < target {
		record, err = s.addLiveNIC(ctx, record, resizer, hotplugger, backend, process)
		if err != nil {
			return record, errdefs.Context(err, "resize sandbox network", reference, "add NIC", "inspect the sandbox and retry", record.Config.NICs != initialNICs)
		}
	}
	for record.Config.NICs > target {
		record, err = s.removeLiveNIC(ctx, record, resizer, hotplugger, backend, process)
		if err != nil {
			return record, errdefs.Context(err, "resize sandbox network", reference, "remove NIC", "inspect the sandbox and retry", record.Config.NICs != initialNICs)
		}
	}
	return record, nil
}

func reconcileNICs(ctx context.Context, record types.Sandbox, target int, provider network.Resizer, hotplugger vmm.NetworkHotplugger, process vmm.Process) error {
	live, err := hotplugger.LiveNICs(ctx, process)
	if err != nil {
		return err
	}
	allocated, err := provider.Allocated(ctx, record.ID)
	if err != nil {
		return err
	}
	byIndex := make(map[int]vmm.NetworkDevice, len(live))
	for _, device := range live {
		index, owned := provider.IndexForTAP(record.ID, device.TAP)
		if !owned || index >= types.MaxSandboxNICs {
			return fmt.Errorf("VMM has unrecognized NIC on TAP %s; refusing to change another device", device.TAP)
		}
		if _, duplicate := byIndex[index]; duplicate {
			return fmt.Errorf("VMM has multiple NICs at slot %d", index)
		}
		byIndex[index] = device
		if index < record.Config.NICs && (device.TAP != record.Network.Interfaces[index].TAP || device.MAC != record.Network.Interfaces[index].MAC) {
			return fmt.Errorf("VMM NIC %d differs from the persisted identity", index)
		}
	}
	for index, device := range byIndex {
		if index >= record.Config.NICs {
			if err := hotplugger.RemoveNIC(ctx, process, device.ID); err != nil {
				return fmt.Errorf("remove uncommitted NIC %d: %w", index, err)
			}
		}
	}
	for _, index := range allocated {
		if index >= record.Config.NICs {
			if err := provider.Remove(ctx, record.ID, index); err != nil {
				return fmt.Errorf("release uncommitted NIC %d: %w", index, err)
			}
		}
	}
	for index := 0; index < min(target, record.Config.NICs); index++ {
		if _, present := byIndex[index]; !present {
			return fmt.Errorf("persisted NIC %d is absent from the VMM; stop and start the sandbox to recover it, or resize below %d", index, index+1)
		}
	}
	return nil
}

func (s *SandboxService) addLiveNIC(ctx context.Context, record types.Sandbox, provider network.Resizer, hotplugger vmm.NetworkHotplugger, backend vmm.Backend, process vmm.Process) (types.Sandbox, error) {
	index := record.Config.NICs
	spec := network.AddSpec{Index: index, Queues: network.QueueCount(record.Config.CPUs)}
	interfaces, err := provider.Add(ctx, record.ID, record.Config.NetworkName, spec)
	if err != nil {
		return record, err
	}
	if len(interfaces) != 1 || interfaces[0].Index != index {
		cleanupCtx, cancel := s.nicCleanupContext(ctx)
		defer cancel()
		return record, errors.Join(fmt.Errorf("network provider returned invalid NIC slot %d", index), provider.Remove(cleanupCtx, record.ID, index))
	}
	device := interfaces[0]
	if err := hotplugger.AddNIC(ctx, process, device); err != nil {
		// The VMM may have accepted the request before the API reply was lost.
		live, inspectErr := hotplugger.LiveNICs(ctx, process)
		if inspectErr != nil || !slices.ContainsFunc(live, func(item vmm.NetworkDevice) bool { return item.TAP == device.TAP && item.MAC == device.MAC }) {
			cleanupCtx, cancel := s.nicCleanupContext(ctx)
			defer cancel()
			return record, errors.Join(err, inspectErr, provider.Remove(cleanupCtx, record.ID, index))
		}
	}
	next := record
	next.Network.Interfaces = append(slices.Clone(record.Network.Interfaces), device)
	next.Config.NICs++
	next.Config.NetworkName = device.Network
	if err := configureGuestNetwork(ctx, backend, process, next, s.dependencies.dnsServers); err != nil {
		return record, errors.Join(err, s.rollbackAddedNIC(ctx, record, provider, hotplugger, backend, process, device))
	}
	committed, err := s.dependencies.catalog.UpdateNetwork(ctx, record.ID, record.Generation, next.Network, next.Config.NetworkName, s.dependencies.now().UTC())
	if err == nil {
		return committed, nil
	}
	inspectCtx, cancel := s.nicCleanupContext(ctx)
	defer cancel()
	current, resolveErr := s.dependencies.catalog.Resolve(inspectCtx, record.ID.String())
	if resolveErr == nil && current.Config.NICs == next.Config.NICs && current.Network.Interfaces[index].TAP == device.TAP {
		return current, nil
	}
	return record, errors.Join(err, resolveErr, s.rollbackAddedNIC(ctx, record, provider, hotplugger, backend, process, device))
}

func (s *SandboxService) rollbackAddedNIC(ctx context.Context, record types.Sandbox, provider network.Resizer, hotplugger vmm.NetworkHotplugger, backend vmm.Backend, process vmm.Process, device types.NetworkInterface) error {
	cleanupCtx, cancel := s.nicCleanupContext(ctx)
	defer cancel()
	live, err := hotplugger.LiveNICs(cleanupCtx, process)
	if err != nil {
		return err
	}
	for _, item := range live {
		if item.TAP == device.TAP {
			if err := hotplugger.RemoveNIC(cleanupCtx, process, item.ID); err != nil {
				return err
			}
		}
	}
	return errors.Join(provider.Remove(cleanupCtx, record.ID, device.Index), configureGuestNetwork(cleanupCtx, backend, process, record, s.dependencies.dnsServers))
}

func (s *SandboxService) removeLiveNIC(ctx context.Context, record types.Sandbox, provider network.Resizer, hotplugger vmm.NetworkHotplugger, backend vmm.Backend, process vmm.Process) (types.Sandbox, error) {
	index := record.Config.NICs - 1
	device := record.Network.Interfaces[index]
	next := record
	next.Network.Interfaces = slices.Clone(record.Network.Interfaces[:index])
	next.Config.NICs = index
	if err := configureGuestNetwork(ctx, backend, process, next, s.dependencies.dnsServers); err != nil {
		return record, err
	}
	if err := quiesceGuestNIC(ctx, backend, process, device); err != nil {
		return record, err
	}
	live, err := hotplugger.LiveNICs(ctx, process)
	if err != nil {
		return record, err
	}
	for _, item := range live {
		if item.TAP == device.TAP {
			if err := hotplugger.RemoveNIC(ctx, process, item.ID); err != nil {
				return record, err
			}
		}
	}
	if err := provider.Remove(ctx, record.ID, index); err != nil {
		return record, err
	}
	committed, err := s.dependencies.catalog.UpdateNetwork(ctx, record.ID, record.Generation, next.Network, next.Config.NetworkName, s.dependencies.now().UTC())
	if err != nil {
		inspectCtx, cancel := s.nicCleanupContext(ctx)
		defer cancel()
		current, resolveErr := s.dependencies.catalog.Resolve(inspectCtx, record.ID.String())
		if resolveErr == nil && current.Config.NICs == index {
			return current, nil
		}
		return record, errors.Join(err, resolveErr)
	}
	return committed, nil
}

func (s *SandboxService) nicCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.dependencies.cleanupTimeout)
}

// runGuestNetworkScript uses the existing host-only agent protocol, so older
// guest images with working exec support need no new agent message type.
func runGuestNetworkScript(ctx context.Context, backend vmm.Backend, process vmm.Process, script string) error {
	connection, err := backend.DialVsock(ctx, process, agent.Port)
	if err != nil {
		return fmt.Errorf("connect guest agent: %w", err)
	}
	code, runErr := agent.Run(ctx, connection, types.Command{Args: []string{"/bin/sh", "-c", script}}, nil, nil, nil)
	return errors.Join(runErr, guestExitError(code), connection.Close())
}

func configureGuestNetwork(ctx context.Context, backend vmm.Backend, process vmm.Process, record types.Sandbox, dns []string) error {
	script, err := cloneGuestScript(record, dns)
	if err != nil {
		return err
	}
	return runGuestNetworkScript(ctx, backend, process, script)
}

func quiesceGuestNIC(ctx context.Context, backend vmm.Backend, process vmm.Process, device types.NetworkInterface) error {
	if err := device.Validate(); err != nil {
		return err
	}
	script := fmt.Sprintf("for net in /sys/class/net/*; do [ \"$(cat \"$net/address\")\" = '%s' ] || continue; ip link set \"${net##*/}\" down; done", device.MAC)
	return runGuestNetworkScript(ctx, backend, process, script)
}
