//go:build linux

package bridge

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
)

var _ network.Resizer = (*Provider)(nil)

const (
	tapGROMaxSize = 65536
	leaseLockName = ".lock"
	ownerDirName  = ".owners"
)

func (p *Provider) queueSizeOrDefault() int {
	if p == nil || p.queueSize <= 0 {
		return network.DefaultQueueSize
	}
	return p.queueSize
}

// New constructs a direct-bridge provider without touching the host.
func New(options Options) (*Provider, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Provider{prefix: options.TAPPrefix, queueSize: options.QueueSize, leaseDir: options.LeaseDir}, nil
}

// Add creates or reclaims one TAP per NIC, attaches it to the requested
// pre-existing bridge, and allocates a stable address from the bridge subnet.
// No CNI plugin or private namespace is involved.
func (p *Provider) Add(ctx context.Context, id types.SandboxID, networkName string, specs ...network.AddSpec) ([]types.NetworkInterface, error) {
	if len(specs) == 0 {
		return []types.NetworkInterface{}, nil
	}
	if networkName == "" {
		return nil, errors.New("bridge device is required")
	}
	bridge, err := lookupBridge(networkName)
	if err != nil {
		return nil, err
	}
	addresses, err := p.reserveAddresses(ctx, id, bridge, specs)
	if err != nil {
		return nil, err
	}
	result := make([]types.NetworkInterface, 0, len(specs))
	for index, spec := range specs {
		item, err := p.addOne(id, bridge, spec, addresses[index])
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(left, right types.NetworkInterface) int { return left.Index - right.Index })
	return result, nil
}

// Verify checks the bridge and every expected TAP attachment without changing
// host state.
func (p *Provider) Verify(_ context.Context, _ types.SandboxID, expected []types.NetworkInterface) error {
	if len(expected) == 0 {
		return nil
	}
	bridge, err := lookupBridge(expected[0].Network)
	if err != nil {
		return err
	}
	for _, item := range expected {
		if err := item.Validate(); err != nil {
			return err
		}
		if item.Network != bridge.Attrs().Name {
			return fmt.Errorf("TAP %s belongs to network %q, expected %q", item.TAP, item.Network, bridge.Attrs().Name)
		}
		link, err := netlink.LinkByName(item.TAP)
		if err != nil {
			return fmt.Errorf("find TAP %s: %w", item.TAP, err)
		}
		if link.Attrs().MasterIndex != bridge.Attrs().Index {
			return fmt.Errorf("TAP %s is not attached to bridge %s", item.TAP, bridge.Attrs().Name)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("TAP %s is down", item.TAP)
		}
	}
	return nil
}

// Recover rebuilds detached or missing TAPs from the persisted guest identity.
func (p *Provider) Recover(ctx context.Context, id types.SandboxID, networkName string, expected []types.NetworkInterface) ([]types.NetworkInterface, error) {
	if len(expected) == 0 {
		return nil, nil
	}
	if networkName == "" {
		networkName = expected[0].Network
	}
	if networkName == "" {
		return nil, errors.New("bridge device is required")
	}
	bridge, err := lookupBridge(networkName)
	if err != nil {
		return nil, err
	}
	specs := make([]network.AddSpec, len(expected))
	for index, item := range expected {
		specs[index] = network.AddSpec{Index: item.Index, DHCP: item.IPv4 == nil, Existing: &expected[index]}
	}
	if _, err := p.reserveAddresses(ctx, id, bridge, specs); err != nil {
		return nil, err
	}
	if err := p.Verify(ctx, id, expected); err == nil {
		return slices.Clone(expected), nil
	}
	result := make([]types.NetworkInterface, 0, len(expected))
	for _, item := range expected {
		if link, lookupErr := netlink.LinkByName(item.TAP); lookupErr == nil {
			if deleteErr := netlink.LinkDel(link); deleteErr != nil {
				return nil, fmt.Errorf("delete stale TAP %s: %w", item.TAP, deleteErr)
			}
		} else if !linkMissing(lookupErr) {
			return nil, fmt.Errorf("find TAP %s: %w", item.TAP, lookupErr)
		}
		recovered, addErr := p.addOne(id, bridge, network.AddSpec{Index: item.Index, Queues: item.Queues, Existing: &item}, item.IPv4)
		if addErr != nil {
			return nil, addErr
		}
		result = append(result, recovered)
	}
	slices.SortFunc(result, func(left, right types.NetworkInterface) int { return left.Index - right.Index })
	return result, nil
}

// Delete removes every TAP named for this sandbox and releases its leases.
func (p *Provider) Delete(_ context.Context, id types.SandboxID) error {
	owned := make(map[string]struct{}, types.MaxSandboxNICs)
	for index := range types.MaxSandboxNICs {
		name, err := network.TAPName(p.prefix, id, index)
		if err != nil {
			return err
		}
		owned[name] = struct{}{}
	}
	var failures []error
	links, err := netlink.LinkList()
	if err != nil {
		failures = append(failures, fmt.Errorf("list host links: %w", err))
	} else {
		for _, link := range links {
			if _, ok := owned[link.Attrs().Name]; !ok {
				continue
			}
			if err := netlink.LinkDel(link); err != nil && !linkMissing(err) {
				failures = append(failures, fmt.Errorf("delete TAP %s: %w", link.Attrs().Name, err))
			}
		}
	}
	if err := p.releaseLeases(id); err != nil {
		failures = append(failures, err)
	}
	if err := p.releaseOwner(id); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// Allocated lists TAP or lease slots owned by the sandbox, including slots
// left by an interrupted resize.
func (p *Provider) Allocated(_ context.Context, id types.SandboxID) ([]int, error) {
	allocated := make(map[int]struct{}, types.MaxSandboxNICs)
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list host links: %w", err)
	}
	for _, link := range links {
		if index, ok := p.indexForTAP(id, link.Attrs().Name); ok {
			allocated[index] = struct{}{}
		}
	}
	entries, err := os.ReadDir(p.leaseDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read bridge lease directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ownerDirName {
			continue
		}
		directory := filepath.Join(p.leaseDir, entry.Name())
		leases, err := os.ReadDir(directory)
		if err != nil {
			return nil, fmt.Errorf("read bridge leases for %s: %w", entry.Name(), err)
		}
		for _, lease := range leases {
			if lease.IsDir() || lease.Name() == leaseLockName {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(directory, lease.Name())) //nolint:gosec // managed lease directory
			if err != nil {
				return nil, fmt.Errorf("read bridge lease %s: %w", lease.Name(), err)
			}
			owner, slot, ok := strings.Cut(string(raw), "/")
			if !ok || owner != id.String() {
				continue
			}
			index, err := strconv.Atoi(slot)
			if err == nil && index >= 0 {
				allocated[index] = struct{}{}
			}
		}
	}
	result := make([]int, 0, len(allocated))
	for index := range allocated {
		result = append(result, index)
	}
	slices.Sort(result)
	return result, nil
}

// IndexForTAP maps one managed TAP name back to its sandbox slot.
func (p *Provider) IndexForTAP(id types.SandboxID, tap string) (int, bool) {
	return p.indexForTAP(id, tap)
}

func (p *Provider) indexForTAP(id types.SandboxID, tap string) (int, bool) {
	for index := range types.MaxSandboxNICs {
		name, err := network.TAPName(p.prefix, id, index)
		if err != nil {
			return 0, false
		}
		if name == tap {
			return index, true
		}
	}
	return 0, false
}

// Remove releases selected live-resize slots while retaining the sandbox owner
// record for a later full Delete.
func (p *Provider) Remove(_ context.Context, id types.SandboxID, indices ...int) error {
	var failures []error
	for _, index := range indices {
		if index < 0 {
			failures = append(failures, fmt.Errorf("negative NIC index %d", index))
			continue
		}
		name, err := network.TAPName(p.prefix, id, index)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		link, err := netlink.LinkByName(name)
		if err == nil {
			if err := netlink.LinkDel(link); err != nil && !linkMissing(err) {
				failures = append(failures, fmt.Errorf("delete TAP %s: %w", name, err))
			}
		} else if !linkMissing(err) {
			failures = append(failures, fmt.Errorf("find TAP %s: %w", name, err))
		}
		if err := p.releaseLeaseIndex(id, index); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (p *Provider) releaseLeaseIndex(id types.SandboxID, index int) error {
	owner := id.String() + "/" + strconv.Itoa(index)
	entries, err := os.ReadDir(p.leaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bridge lease directory: %w", err)
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ownerDirName {
			continue
		}
		directory := filepath.Join(p.leaseDir, entry.Name())
		leases, err := os.ReadDir(directory)
		if err != nil {
			failures = append(failures, fmt.Errorf("read bridge leases for %s: %w", entry.Name(), err))
			continue
		}
		for _, lease := range leases {
			if lease.IsDir() || lease.Name() == leaseLockName {
				continue
			}
			path := filepath.Join(directory, lease.Name())
			raw, err := os.ReadFile(path) //nolint:gosec // managed lease directory
			if err != nil {
				failures = append(failures, fmt.Errorf("read bridge lease %s: %w", path, err))
				continue
			}
			if strings.TrimSpace(string(raw)) == owner {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					failures = append(failures, fmt.Errorf("release bridge lease %s: %w", path, err))
				}
			}
		}
	}
	return errors.Join(failures...)
}

// OwnedIDs lists sandbox identities with a bridge lease so cross-provider GC
// can reclaim TAPs and addresses left by a lost sandbox record.
func (p *Provider) OwnedIDs(ctx context.Context) ([]types.SandboxID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p.leaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bridge lease directory: %w", err)
	}
	owned := make(map[types.SandboxID]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(p.leaseDir, entry.Name())
		leases, err := os.ReadDir(directory)
		if err != nil {
			return nil, fmt.Errorf("read bridge leases for %s: %w", entry.Name(), err)
		}
		if owners, err := os.ReadDir(filepath.Join(directory, ownerDirName)); err == nil {
			for _, owner := range owners {
				id, err := types.ParseSandboxID(owner.Name())
				if err != nil {
					continue
				}
				owned[id] = struct{}{}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read bridge owners for %s: %w", entry.Name(), err)
		}
		for _, lease := range leases {
			if lease.Name() == leaseLockName || lease.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(directory, lease.Name())) //nolint:gosec // managed lease directory
			if err != nil {
				return nil, fmt.Errorf("read bridge lease %s: %w", lease.Name(), err)
			}
			owner, _, _ := strings.Cut(string(raw), "/")
			id, err := types.ParseSandboxID(owner)
			if err != nil {
				continue
			}
			owned[id] = struct{}{}
		}
	}
	result := make([]types.SandboxID, 0, len(owned))
	for id := range owned {
		result = append(result, id)
	}
	slices.SortFunc(result, func(left, right types.SandboxID) int {
		return strings.Compare(left.String(), right.String())
	})
	return result, nil
}

func (p *Provider) addOne(id types.SandboxID, bridge netlink.Link, spec network.AddSpec, ipv4 *types.IPv4Config) (types.NetworkInterface, error) {
	tapName, err := network.TAPName(p.prefix, id, spec.Index)
	if err != nil {
		return types.NetworkInterface{}, err
	}
	queues := spec.Queues
	if queues <= 0 {
		queues = network.QueueCount(1)
	}
	if queues < 2 || queues%2 != 0 {
		return types.NetworkInterface{}, fmt.Errorf("NIC %d requires an even queue count of at least two", spec.Index)
	}
	mac := ""
	var existing *types.NetworkInterface
	if spec.Existing != nil {
		existing = spec.Existing
		if existing.TAP != "" && existing.TAP != tapName {
			return types.NetworkInterface{}, fmt.Errorf("TAP %s differs from provider name %s", existing.TAP, tapName)
		}
		if existing.Network != "" && existing.Network != bridge.Attrs().Name {
			return types.NetworkInterface{}, fmt.Errorf("TAP %s belongs to bridge %s, expected %s", tapName, existing.Network, bridge.Attrs().Name)
		}
		mac = existing.MAC
	}
	if mac == "" {
		mac, err = randomMAC()
		if err != nil {
			return types.NetworkInterface{}, err
		}
	}
	if existing != nil {
		if link, lookupErr := netlink.LinkByName(tapName); lookupErr == nil {
			if link.Attrs().MasterIndex == bridge.Attrs().Index && link.Attrs().Flags&net.FlagUp != 0 {
				return providerInterface(tapName, bridge.Attrs().Name, mac, ipv4, queues, p.queueSizeOrDefault(), spec.Index), nil
			}
			if err := netlink.LinkDel(link); err != nil {
				return types.NetworkInterface{}, fmt.Errorf("delete stale TAP %s: %w", tapName, err)
			}
		} else if !linkMissing(lookupErr) {
			return types.NetworkInterface{}, fmt.Errorf("find TAP %s: %w", tapName, lookupErr)
		}
	}
	tap, err := createTap(tapName, queues, bridge.Attrs().MTU)
	if err != nil {
		return types.NetworkInterface{}, err
	}
	if err := attachToBridge(tap, bridge.Attrs().Index, bridge.Attrs().MTU); err != nil {
		_ = netlink.LinkDel(tap)
		return types.NetworkInterface{}, err
	}
	return providerInterface(tapName, bridge.Attrs().Name, mac, ipv4, queues, p.queueSizeOrDefault(), spec.Index), nil
}

func providerInterface(tapName, bridgeName, mac string, ipv4 *types.IPv4Config, queues, queueSize, index int) types.NetworkInterface {
	return types.NetworkInterface{
		Index: index, Name: fmt.Sprintf("eth%d", index), TAP: tapName,
		MAC: mac, Queues: queues, QueueSize: queueSize, Network: bridgeName, IPv4: ipv4,
	}
}

func lookupBridge(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("find bridge %s: %w", name, err)
	}
	if link.Type() != "bridge" {
		return nil, fmt.Errorf("%s is not a Linux bridge (type %q)", name, link.Type())
	}
	return link, nil
}

// bridgeIPv4 derives the guest gateway, subnet and prefix from the bridge's
// first IPv4 address. KumaBox does not create or renumber the bridge.
func bridgeIPv4(bridge netlink.Link) (net.IP, *net.IPNet, int, error) {
	addresses, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list IPv4 addresses on %s: %w", bridge.Attrs().Name, err)
	}
	for _, address := range addresses {
		if address.IP == nil || address.IP.To4() == nil || address.IPNet == nil {
			continue
		}
		ones, bits := address.Mask.Size()
		if bits != 32 || ones < 0 {
			continue
		}
		return address.IP.To4(), &net.IPNet{IP: address.IP.Mask(address.Mask), Mask: address.Mask}, ones, nil
	}
	return nil, nil, 0, fmt.Errorf("bridge %s has no IPv4 address; assign its gateway address before using --bridge", bridge.Attrs().Name)
}

func (p *Provider) allocateAddress(bridgeName string, subnet *net.IPNet, gateway net.IP, id types.SandboxID, index int) (string, error) {
	base := binary.BigEndian.Uint32(subnet.IP.To4())
	mask := binary.BigEndian.Uint32(subnet.Mask)
	first := (base & mask) + 1
	last := (base & mask) | ^mask
	gatewayValue := binary.BigEndian.Uint32(gateway.To4())
	for candidate := first; candidate < last; candidate++ {
		if candidate == gatewayValue {
			continue
		}
		address := uint32ToIP(candidate).String()
		if _, err := os.Stat(p.leasePath(bridgeName, address)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect bridge lease %s: %w", address, err)
		}
		if err := p.claimLease(bridgeName, address, id, index); err == nil {
			return address, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("bridge %s subnet %s has no free address", bridgeName, subnet.String())
}

func (p *Provider) claimLease(bridgeName, address string, id types.SandboxID, index int) error {
	if err := p.ensureLeaseDir(bridgeName); err != nil {
		return err
	}
	owner := id.String() + "/" + strconv.Itoa(index)
	path := p.leasePath(bridgeName, address)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // managed lease directory
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("claim bridge address %s: %w", address, err)
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // managed lease directory
		if readErr != nil {
			return fmt.Errorf("read bridge lease %s: %w", address, readErr)
		}
		if string(raw) == owner {
			return nil
		}
		return fmt.Errorf("%w: bridge address %s is already leased by %s", os.ErrExist, address, strings.TrimSpace(string(raw)))
	}
	_, writeErr := file.WriteString(owner)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func (p *Provider) releaseLeases(id types.SandboxID) error {
	entries, err := os.ReadDir(p.leaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bridge lease directory: %w", err)
	}
	prefix := id.String() + "/"
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(p.leaseDir, entry.Name())
		leases, err := os.ReadDir(directory)
		if err != nil {
			failures = append(failures, fmt.Errorf("read bridge leases for %s: %w", entry.Name(), err))
			continue
		}
		for _, lease := range leases {
			if lease.Name() == leaseLockName || lease.IsDir() {
				continue
			}
			path := filepath.Join(directory, lease.Name())
			raw, err := os.ReadFile(path) //nolint:gosec // managed lease directory
			if err != nil {
				failures = append(failures, fmt.Errorf("read bridge lease %s: %w", path, err))
				continue
			}
			if strings.HasPrefix(string(raw), prefix) {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					failures = append(failures, fmt.Errorf("release bridge lease %s: %w", path, err))
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (p *Provider) claimOwner(bridgeName string, id types.SandboxID) error {
	directory := filepath.Join(p.leaseDir, bridgeName, ownerDirName)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create bridge owner directory: %w", err)
	}
	path := filepath.Join(directory, id.String())
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // managed lease directory
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim bridge owner: %w", err)
	}
	return errors.Join(file.Sync(), file.Close())
}

func (p *Provider) releaseOwner(id types.SandboxID) error {
	entries, err := os.ReadDir(p.leaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bridge lease directory: %w", err)
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(p.leaseDir, entry.Name(), ownerDirName, id.String())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("release bridge owner %s: %w", path, err))
		}
	}
	return errors.Join(failures...)
}

// reserveAddresses keeps per-sandbox ownership durable before TAP creation.
// DHCP has no shared host-side address allocation, so its owner fsync must not
// hold the bridge-wide IPAM lock or serialize unrelated sandbox creation.
func (p *Provider) reserveAddresses(
	ctx context.Context,
	id types.SandboxID,
	bridge netlink.Link,
	specs []network.AddSpec,
) (result []*types.IPv4Config, returnErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := bridge.Attrs().Name
	if err := p.claimOwner(name, id); err != nil {
		return nil, err
	}
	result = make([]*types.IPv4Config, len(specs))
	if !slices.ContainsFunc(specs, func(spec network.AddSpec) bool { return !spec.DHCP }) {
		return result, nil
	}
	gateway, subnet, prefix, err := bridgeIPv4(bridge)
	if err != nil {
		return nil, err
	}
	guard, err := p.lockLeases(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Unlock(context.WithoutCancel(ctx))) }()
	for index, spec := range specs {
		if spec.DHCP {
			continue
		}
		if spec.Existing != nil && spec.Existing.IPv4 != nil {
			address := *spec.Existing.IPv4
			if !subnet.Contains(net.ParseIP(address.Address)) {
				return nil, fmt.Errorf("NIC %d address %s is outside bridge %s subnet", spec.Index, address.Address, name)
			}
			if err := p.claimLease(name, address.Address, id, spec.Index); err != nil {
				return nil, err
			}
			result[index] = &address
			continue
		}
		address, err := p.allocateAddress(name, subnet, gateway, id, spec.Index)
		if err != nil {
			return nil, err
		}
		result[index] = &types.IPv4Config{Address: address, Gateway: gateway.String(), Prefix: prefix}
	}
	return result, nil
}

func (p *Provider) lockLeases(ctx context.Context, bridgeName string) (*filelock.Lock, error) {
	if err := p.ensureLeaseDir(bridgeName); err != nil {
		return nil, err
	}
	guard := filelock.New(filepath.Join(p.leaseDir, bridgeName, leaseLockName))
	if err := guard.Lock(ctx); err != nil {
		return nil, fmt.Errorf("lock bridge leases: %w", err)
	}
	return guard, nil
}

func (p *Provider) ensureLeaseDir(bridgeName string) error {
	if err := os.MkdirAll(filepath.Join(p.leaseDir, bridgeName), 0o750); err != nil {
		return fmt.Errorf("create bridge lease directory: %w", err)
	}
	return nil
}

func (p *Provider) leasePath(bridgeName, address string) string {
	return filepath.Join(p.leaseDir, bridgeName, address)
}

func uint32ToIP(value uint32) net.IP {
	buffer := make([]byte, 4)
	binary.BigEndian.PutUint32(buffer, value)
	return net.IP(buffer)
}

func createTap(name string, queues, mtu int) (netlink.Link, error) {
	queuePairs := max(1, queues/2)
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs == 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	attrs := netlink.LinkAttrs{Name: name, Flags: net.FlagUp, TxQLen: -1}
	if mtu > 0 {
		attrs.MTU = mtu
	}
	tap := &netlink.Tuntap{
		LinkAttrs: attrs,
		Mode:      netlink.TUNTAP_MODE_TAP,
		Queues:    queuePairs,
		Flags:     flags,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create TAP %s: %w", name, err)
	}
	for _, descriptor := range tap.Fds {
		_ = descriptor.Close()
	}
	link := netlink.Link(tap)
	if tap.Attrs().Index == 0 {
		resolved, err := netlink.LinkByName(name)
		if err != nil {
			return nil, fmt.Errorf("resolve TAP %s: %w", name, err)
		}
		link = resolved
	}
	_ = netlink.LinkSetGROMaxSize(link, tapGROMaxSize)
	return link, nil
}

// attachToBridge sets master, MTU and IFF_UP in one RTM_SETLINK, so a NIC pays
// the node-wide RTNL lock once instead of once per attribute.
func attachToBridge(link netlink.Link, bridgeIndex, mtu int) error {
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(link.Attrs().Index) //nolint:gosec // kernel link indices are positive int32
	msg.Flags = unix.IFF_UP
	msg.Change = unix.IFF_UP
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_MASTER, nl.Uint32Attr(uint32(bridgeIndex)))) //nolint:gosec // resolved kernel bridge index
	if mtu > 0 {
		req.AddData(nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(mtu)))) //nolint:gosec // positive MTU read from the existing bridge
	}
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("attach TAP %s to bridge %d: %w", link.Attrs().Name, bridgeIndex, err)
	}
	return nil
}

func randomMAC() (string, error) {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate guest MAC: %w", err)
	}
	buffer[0] = (buffer[0] | 0x02) & 0xfe
	return net.HardwareAddr(buffer).String(), nil
}

func linkMissing(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}
