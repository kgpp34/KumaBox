package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
)

// Prepare records namespace ownership before asking the kernel to create it.
// If no conflist is installed, it returns an empty namespace; Add will report
// the actionable configuration error when networking is actually requested.
func (p *Provider) Prepare(ctx context.Context, id types.SandboxID) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	if _, err := p.confList(""); err != nil {
		if errors.Is(err, network.ErrNotConfigured) {
			return "", nil
		}
		return "", err
	}
	name, path := p.namespace(id)
	if err := p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return &recordData{
				Version: recordVersion, SandboxID: id.String(), NamespaceName: name,
				NamespacePath: path, Phase: phasePreparing, Interfaces: []interfaceData{},
			}, nil
		}
		if record.Phase == phaseDeleting {
			return nil, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s network deletion is incomplete", id))
		}
		if record.NamespaceName != name || record.NamespacePath != path {
			return nil, corrupt(errors.New("stored namespace differs from configured namespace"))
		}
		return record, nil
	}); err != nil {
		return "", fmt.Errorf("record network namespace intent: %w", err)
	}
	if _, err := p.platform.EnsureNamespace(name, path); err != nil {
		return "", fmt.Errorf("ensure network namespace %s: %w", name, err)
	}
	return path, nil
}

// Add stages every NIC before plugin execution, then advances one interface at
// a time through adding to ready. A crash during ADD therefore leaves enough
// information for Delete to issue the matching DEL.
//
//	staged -> adding -> CNI ADD -> TAP/TC -> ready
//	              \---- failure ----> CNI DEL -> sweep
func (p *Provider) Add(ctx context.Context, id types.SandboxID, networkName string, specs ...network.AddSpec) (result []types.NetworkInterface, returnErr error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return []types.NetworkInterface{}, nil
	}
	list, err := p.confList(networkName)
	if err != nil {
		return nil, err
	}
	if _, err := p.Prepare(ctx, id); err != nil {
		return nil, err
	}
	if err := validateSpecs(specs); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := p.stage(ctx, id, list.Name, specs); err != nil {
		return nil, err
	}

	touched := make([]int, 0, len(specs))
	defer func() {
		if returnErr == nil || len(touched) == 0 {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.options.CleanupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, p.rollback(rollbackCtx, id, list, touched))
	}()

	result = make([]types.NetworkInterface, 0, len(specs))
	for _, spec := range specs {
		item, err := p.interfaceRecord(ctx, id, spec.Index)
		if err != nil {
			return nil, err
		}
		if item.Phase == interfaceReady {
			ready, err := item.toType(list.Name)
			if err != nil {
				return nil, corrupt(err)
			}
			result = append(result, ready)
			continue
		}
		touched = append(touched, spec.Index)
		if item.Phase == interfaceAdding {
			if err := p.deleteOne(ctx, id, list, item, true); err != nil {
				return nil, fmt.Errorf("recover interrupted CNI ADD for %s/%s: %w", id, item.Name, err)
			}
		}
		if err := p.setInterfacePhase(ctx, id, spec.Index, interfaceAdding); err != nil {
			return nil, err
		}
		ready, err := p.addOne(ctx, id, list, item, spec.Existing)
		if err != nil {
			return nil, err
		}
		if err := p.storeReady(ctx, id, ready); err != nil {
			return nil, err
		}
		result = append(result, ready)
	}
	if err := p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return nil, corrupt(errors.New("network record disappeared while completing ADD"))
		}
		record.Phase = phaseReady
		return record, nil
	}); err != nil {
		return nil, fmt.Errorf("commit network readiness: %w", err)
	}
	touched = nil
	slices.SortFunc(result, func(left, right types.NetworkInterface) int { return left.Index - right.Index })
	return result, nil
}

func (p *Provider) stage(ctx context.Context, id types.SandboxID, networkName string, specs []network.AddSpec) error {
	return p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return nil, corrupt(errors.New("network namespace intent is missing"))
		}
		if record.Phase == phaseDeleting {
			return nil, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s network deletion is incomplete", id))
		}
		if record.Network != "" && record.Network != networkName {
			return nil, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is already bound to CNI network %q", id, record.Network))
		}
		record.Network = networkName
		for _, spec := range specs {
			position := findInterface(record, spec.Index)
			if position >= 0 {
				continue
			}
			tap, err := network.TAPName(defaultTAPPrefix, id, spec.Index)
			if err != nil {
				return nil, err
			}
			value := types.NetworkInterface{
				Index: spec.Index, Name: interfaceName(spec.Index), TAP: tap,
				Queues: spec.Queues, QueueSize: network.DefaultQueueSize, Network: networkName,
			}
			if spec.Existing != nil {
				value.MAC = spec.Existing.MAC
				value.IPv4 = spec.Existing.IPv4
			}
			record.Interfaces = append(record.Interfaces, fromType(value, interfaceStaged))
		}
		slices.SortFunc(record.Interfaces, func(left, right interfaceData) int { return left.Index - right.Index })
		return record, nil
	})
}

func (p *Provider) addOne(ctx context.Context, id types.SandboxID, list *libcni.NetworkConfigList, item interfaceData, existing *types.NetworkInterface) (types.NetworkInterface, error) {
	record, err := p.view(ctx, id)
	if err != nil || record == nil {
		return types.NetworkInterface{}, errors.Join(err, errors.New("network record is missing"))
	}
	runtimeConfig := &libcni.RuntimeConf{ContainerID: id.String(), NetNS: record.NamespacePath, IfName: item.Name}
	if existing != nil && existing.IPv4 != nil && existing.IPv4.Address != "" {
		runtimeConfig.Args = [][2]string{{"IgnoreUnknown", "1"}, {"IP", existing.IPv4.Address}}
	}
	cniResult, err := p.runtime.AddNetworkList(ctx, list, runtimeConfig)
	if err != nil {
		return types.NetworkInterface{}, fmt.Errorf("CNI ADD %s/%s: %w", id, item.Name, err)
	}
	ipv4, err := extractIPv4(cniResult)
	if err != nil {
		return types.NetworkInterface{}, fmt.Errorf("parse CNI result for %s/%s: %w", id, item.Name, err)
	}
	overrideMAC := item.MAC
	if existing != nil && existing.MAC != "" {
		overrideMAC = existing.MAC
	}
	mac, err := p.platform.SetupRedirect(record.NamespacePath, item.Name, item.TAP, item.Queues, overrideMAC)
	if err != nil {
		return types.NetworkInterface{}, fmt.Errorf("connect TAP for %s/%s: %w", id, item.Name, err)
	}
	ready := types.NetworkInterface{
		Index: item.Index, Name: item.Name, TAP: item.TAP, MAC: mac,
		Queues: item.Queues, QueueSize: item.QueueSize, Network: list.Name, IPv4: ipv4,
	}
	if err := ready.Validate(); err != nil {
		return types.NetworkInterface{}, err
	}
	return ready, nil
}

func (p *Provider) rollback(ctx context.Context, id types.SandboxID, list *libcni.NetworkConfigList, indices []int) error {
	var failures []error
	released := make(map[int]bool, len(indices))
	for _, index := range indices {
		item, err := p.interfaceRecord(ctx, id, index)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := p.deleteOne(ctx, id, list, item, true); err != nil {
			failures = append(failures, fmt.Errorf("rollback %s: %w", item.Name, err))
			continue
		}
		released[index] = true
	}
	if len(released) > 0 {
		if err := p.update(ctx, id, func(record *recordData) (*recordData, error) {
			if record == nil {
				return nil, nil
			}
			for index := range released {
				removeInterface(record, index)
			}
			return record, nil
		}); err != nil {
			failures = append(failures, fmt.Errorf("release rollback records: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (p *Provider) deleteOne(ctx context.Context, id types.SandboxID, list *libcni.NetworkConfigList, item interfaceData, deleteTAP bool) error {
	record, err := p.view(ctx, id)
	if err != nil || record == nil {
		return errors.Join(err, errors.New("network record is missing"))
	}
	if item.Phase != interfaceStaged {
		runtimeConfig := &libcni.RuntimeConf{ContainerID: id.String(), NetNS: record.NamespacePath, IfName: item.Name}
		if err := p.runtime.DelNetworkList(ctx, list, runtimeConfig); err != nil {
			return fmt.Errorf("CNI DEL %s/%s: %w", id, item.Name, err)
		}
	}
	if deleteTAP {
		if err := p.platform.DeleteTAP(record.NamespacePath, item.TAP); err != nil {
			return fmt.Errorf("delete TAP %s: %w", item.TAP, err)
		}
	}
	return nil
}

func (p *Provider) setInterfacePhase(ctx context.Context, id types.SandboxID, index int, phase interfacePhase) error {
	return p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return nil, corrupt(errors.New("network record is missing"))
		}
		position := findInterface(record, index)
		if position < 0 {
			return nil, corrupt(fmt.Errorf("network interface %d is missing", index))
		}
		record.Interfaces[position].Phase = phase
		return record, nil
	})
}

func (p *Provider) storeReady(ctx context.Context, id types.SandboxID, ready types.NetworkInterface) error {
	return p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return nil, corrupt(errors.New("network record is missing"))
		}
		position := findInterface(record, ready.Index)
		if position < 0 {
			return nil, corrupt(fmt.Errorf("network interface %d is missing", ready.Index))
		}
		record.Interfaces[position] = fromType(ready, interfaceReady)
		return record, nil
	})
}

func (p *Provider) interfaceRecord(ctx context.Context, id types.SandboxID, index int) (interfaceData, error) {
	record, err := p.view(ctx, id)
	if err != nil {
		return interfaceData{}, err
	}
	if record == nil {
		return interfaceData{}, corrupt(errors.New("network record is missing"))
	}
	position := findInterface(record, index)
	if position < 0 {
		return interfaceData{}, corrupt(fmt.Errorf("network interface %d is missing", index))
	}
	return record.Interfaces[position], nil
}

// Verify checks both the namespace and every expected TAP. Metadata alone is
// never accepted as proof that host plumbing survived a reboot.
func (p *Provider) Verify(_ context.Context, id types.SandboxID, expected []types.NetworkInterface) error {
	if err := validID(id); err != nil {
		return err
	}
	_, path := p.namespace(id)
	if err := p.platform.NamespaceExists(path); err != nil {
		return fmt.Errorf("network namespace %s: %w", path, err)
	}
	for _, item := range expected {
		if err := item.Validate(); err != nil {
			return err
		}
		if err := p.platform.VerifyTAP(path, item.TAP); err != nil {
			return fmt.Errorf("verify TAP %s: %w", item.TAP, err)
		}
	}
	return nil
}

// Recover rebuilds missing host plumbing from the durable guest identities.
func (p *Provider) Recover(ctx context.Context, id types.SandboxID, networkName string, expected []types.NetworkInterface) ([]types.NetworkInterface, error) {
	if err := p.Verify(ctx, id, expected); err == nil {
		if err := p.Unquiesce(ctx, id); err != nil {
			return nil, err
		}
		return slices.Clone(expected), nil
	}
	if err := p.Delete(ctx, id); err != nil {
		return nil, fmt.Errorf("delete incomplete network before recovery: %w", err)
	}
	if _, err := p.Prepare(ctx, id); err != nil {
		return nil, err
	}
	specs := make([]network.AddSpec, len(expected))
	for index := range expected {
		current := expected[index]
		specs[index] = network.AddSpec{Index: current.Index, Queues: current.Queues, Existing: &current}
		if networkName == "" {
			networkName = current.Network
		}
	}
	return p.Add(ctx, id, networkName, specs...)
}

// Quiesce brings CNI-side veth devices down while retaining identity and TAPs.
func (p *Provider) Quiesce(ctx context.Context, id types.SandboxID) error {
	return p.setLinkState(ctx, id, false)
}

// Unquiesce brings retained CNI-side veth devices back up before launch.
func (p *Provider) Unquiesce(ctx context.Context, id types.SandboxID) error {
	return p.setLinkState(ctx, id, true)
}

func (p *Provider) setLinkState(ctx context.Context, id types.SandboxID, up bool) error {
	record, err := p.view(ctx, id)
	if err != nil || record == nil {
		return err
	}
	names := make([]string, 0, len(record.Interfaces))
	for _, item := range record.Interfaces {
		if item.Phase == interfaceReady {
			names = append(names, item.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	if err := p.platform.SetLinkState(record.NamespacePath, names, up); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("set sandbox %s network links up=%t: %w", id, up, err)
	}
	return nil
}

// Delete advances the aggregate to deleting before slow host operations. Each
// successful DEL is swept independently; failures keep exactly the remaining
// release context for the next retry.
//
//	ready -> deleting -> per-NIC DEL -> remove netns -> delete record
//	                         \ failure: retain only unfinished NICs /
func (p *Provider) Delete(ctx context.Context, id types.SandboxID) error {
	if err := validID(id); err != nil {
		return err
	}
	if err := p.update(ctx, id, func(record *recordData) (*recordData, error) {
		if record == nil {
			return nil, nil
		}
		record.Phase = phaseDeleting
		return record, nil
	}); err != nil {
		return fmt.Errorf("mark network deleting: %w", err)
	}
	record, err := p.view(ctx, id)
	if err != nil || record == nil {
		return err
	}
	released := make(map[int]bool, len(record.Interfaces))
	var failures []error
	for _, item := range record.Interfaces {
		list, listErr := p.confList(record.Network)
		if item.Phase == interfaceStaged {
			listErr = nil
		}
		if listErr != nil {
			failures = append(failures, listErr)
			continue
		}
		if err := p.deleteOne(ctx, id, list, item, false); err != nil {
			failures = append(failures, err)
			continue
		}
		released[item.Index] = true
	}
	if len(released) > 0 {
		if err := p.update(ctx, id, func(current *recordData) (*recordData, error) {
			if current == nil {
				return nil, nil
			}
			for index := range released {
				removeInterface(current, index)
			}
			return current, nil
		}); err != nil {
			failures = append(failures, fmt.Errorf("sweep released network records: %w", err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if err := p.platform.RemoveNamespace(ctx, record.NamespaceName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove network namespace %s: %w", record.NamespaceName, err)
	}
	if err := p.update(ctx, id, func(*recordData) (*recordData, error) { return nil, nil }); err != nil {
		return fmt.Errorf("finalize network deletion: %w", err)
	}
	return nil
}

func validateSpecs(specs []network.AddSpec) error {
	seen := make(map[int]struct{}, len(specs))
	for _, spec := range specs {
		if spec.Index < 0 || spec.Queues < 2 || spec.Queues%2 != 0 {
			return fmt.Errorf("NIC %d requires an even queue count of at least two", spec.Index)
		}
		if _, exists := seen[spec.Index]; exists {
			return fmt.Errorf("NIC index %d is duplicated", spec.Index)
		}
		seen[spec.Index] = struct{}{}
		if spec.Existing != nil {
			if spec.Existing.Index != spec.Index {
				return fmt.Errorf("NIC %d recovery identity belongs to index %d", spec.Index, spec.Existing.Index)
			}
			if err := spec.Existing.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func validID(id types.SandboxID) error {
	_, err := types.ParseSandboxID(id.String())
	if err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return nil
}

func extractIPv4(result cnitypes.Result) (*types.IPv4Config, error) {
	converted, err := current.NewResultFromResult(result)
	if err != nil {
		return nil, err
	}
	for _, configuration := range converted.IPs {
		if configuration == nil || configuration.Address.IP.To4() == nil {
			continue
		}
		prefix, _ := configuration.Address.Mask.Size()
		result := &types.IPv4Config{Address: configuration.Address.IP.String(), Prefix: prefix}
		if configuration.Gateway != nil {
			result.Gateway = configuration.Gateway.String()
		}
		return result, result.Validate()
	}
	return nil, nil
}
