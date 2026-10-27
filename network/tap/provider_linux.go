//go:build linux

package tap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

var (
	_ network.Resizer          = (*Provider)(nil)
	_ network.GarbageCollector = (*Provider)(nil)
)

// Prepare validates host prerequisites without allocating a namespace. Fixed
// guest addresses are isolated by TAP translation, not by a shared bridge.
func (p *Provider) Prepare(ctx context.Context, _ types.SandboxID) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := storage.EnsureDir(p.options.StateDir); err != nil {
		return "", err
	}
	value, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil || len(value) == 0 || value[0] != '1' {
		return "", errors.Join(errors.New("eBPF networking requires host setup; run kumabox-ebpf setup"), err)
	}
	return "", nil
}

func (p *Provider) dataPlane(ctx context.Context) (result *datapath, returnErr error) {
	if err := storage.EnsureDir(p.options.StateDir); err != nil {
		return nil, err
	}
	guard := filelock.New(filepath.Join(p.options.StateDir, ".datapath.lock"))
	if err := guard.Lock(ctx); err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Unlock(context.WithoutCancel(ctx))) }()
	return p.openDatapath()
}

// Add publishes durable leases first, installs both fail-closed TC filters,
// then enables packet translation. Interrupted changes remain owned for GC.
func (p *Provider) Add(ctx context.Context, id types.SandboxID, name string, specs ...network.AddSpec) ([]types.NetworkInterface, error) {
	if name != "" && name != "ebpf" {
		return nil, errors.New("eBPF network name must be ebpf or empty")
	}
	if _, err := p.Prepare(ctx, id); err != nil {
		return nil, err
	}
	d, err := p.dataPlane(ctx)
	if err != nil {
		return nil, err
	}
	defer d.close() //nolint:errcheck // fd release, attached programs remain pinned
	result := make([]types.NetworkInterface, 0, len(specs))
	for _, spec := range specs {
		item, err := p.reserve(ctx, id, spec)
		if err != nil {
			return nil, err
		}
		if err := p.ensureTAP(item, d); err != nil {
			return nil, err
		}
		result = append(result, item.Interface)
	}
	return result, nil
}

func (p *Provider) alias(item lease) string {
	return "kumabox:" + p.owner + ":" + item.Sandbox.String() + ":" + fmt.Sprint(item.Interface.Index)
}

func (p *Provider) ownedLink(item lease) (netlink.Link, error) {
	link, err := netlink.LinkByName(item.Interface.TAP)
	if err != nil {
		return nil, err
	}
	if link.Type() != "tuntap" || link.Attrs().Alias != p.alias(item) {
		return nil, fmt.Errorf("refuse unrelated interface %s: ownership differs", item.Interface.TAP)
	}
	return link, nil
}

func (p *Provider) ensureTAP(item lease, d *datapath) error {
	link, err := p.ownedLink(item)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		attrs := netlink.NewLinkAttrs()
		attrs.Name, attrs.Alias, attrs.MTU, attrs.TxQLen = item.Interface.TAP, p.alias(item), 1500, 1000
		tap := &netlink.Tuntap{
			LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TAP,
			NonPersist: true,
			Flags:      tapQueueFlags(item.Interface.Queues),
			Queues:     max(1, item.Interface.Queues/2),
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("create fixed TAP: %w", err)
		}
		defer func() {
			for _, fd := range tap.Fds {
				_ = fd.Close()
			}
		}()
		// TUN creation uses ioctl and does not apply LinkAttrs.Alias. Keep the
		// TAP nonpersistent until ownership is installed: a crash before this
		// point closes the descriptors and removes the unowned interface.
		if err := netlink.LinkSetAlias(tap, p.alias(item)); err != nil {
			return err
		}
		if err := netlink.LinkSetMTU(tap, 1500); err != nil {
			return err
		}
		if err := netlink.LinkSetTxQLen(tap, 1000); err != nil {
			return err
		}
		if len(tap.Fds) == 0 {
			return errors.New("TAP creation returned no queue descriptors")
		}
		if err := unix.IoctlSetInt(int(tap.Fds[0].Fd()), unix.TUNSETPERSIST, 1); err != nil {
			return err
		}
		link, err = p.ownedLink(item)
	}
	if err != nil {
		return err
	}
	value, err := p.endpoint(item.Interface, false)
	if err != nil {
		return err
	}
	key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface indices are positive int32
	if err := d.endpoints.Update(key, value, ebpf.UpdateAny); err != nil {
		return err
	}
	if err := attachFilters(link, d); err != nil {
		return err
	}
	if err := netlink.LinkSetHardwareAddr(link, net.HardwareAddr(value.HostMAC[:])); err != nil {
		return fmt.Errorf("set fixed gateway MAC: %w", err)
	}
	// Neighbour installation requires IFF_UP. Both TC filters are already
	// installed and the endpoint remains disabled until setup is complete.
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring TAP up: %w", err)
	}
	// A /32 gateway is local to this isolated TAP. A permanent neighbour and
	// host route address the unique outside endpoint; no guest-side ARP changes.
	address := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(value.Gateway[:]), Mask: net.CIDRMask(32, 32)}}
	if err := netlink.AddrReplace(link, address); err != nil {
		return fmt.Errorf("install TAP gateway: %w", err)
	}
	if err := netlink.NeighSet(&netlink.Neigh{
		LinkIndex: link.Attrs().Index, Family: netlink.FAMILY_V4,
		State: netlink.NUD_PERMANENT, IP: net.ParseIP(item.Interface.HostAddress), HardwareAddr: net.HardwareAddr(value.GuestMAC[:]),
	}); err != nil {
		return fmt.Errorf("install TAP neighbour: %w", err)
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: net.ParseIP(item.Interface.HostAddress), Mask: net.CIDRMask(32, 32)}, Scope: netlink.SCOPE_LINK,
	}
	if err := addOwnedRoute(route); err != nil {
		return fmt.Errorf("install TAP host route: %w", err)
	}
	value.Enabled = 1
	return d.endpoints.Update(key, value, ebpf.UpdateAny)
}

// tapQueueFlags matches the monitor's TUNSETIFF contract: a single RX/TX
// pair opens without IFF_MULTI_QUEUE, while additional pairs require it.
// Creating every TAP as multiqueue makes single-vCPU monitors reject it.
func tapQueueFlags(queues int) netlink.TuntapFlag {
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queues <= 2 {
		return flags | netlink.TUNTAP_ONE_QUEUE
	}
	return flags | netlink.TUNTAP_MULTI_QUEUE
}

// addOwnedRoute never replaces another installation's endpoint route, even
// if an operator accidentally configures overlapping host address pools.
func addOwnedRoute(route *netlink.Route) error {
	err := netlink.RouteAdd(route)
	if !errors.Is(err, unix.EEXIST) {
		return err
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, route, netlink.RT_FILTER_DST)
	if err != nil {
		return err
	}
	for _, existing := range routes {
		if existing.LinkIndex == route.LinkIndex && existing.Scope == route.Scope {
			return nil
		}
	}
	return fmt.Errorf("host endpoint %s already has a route owned by another interface", route.Dst)
}

// Verify checks ownership, translation, and the attached kernel program IDs.
func (p *Provider) Verify(ctx context.Context, id types.SandboxID, expected []types.NetworkInterface) error {
	if len(expected) == 0 {
		return nil
	}
	d, err := p.dataPlane(ctx)
	if err != nil {
		return err
	}
	defer d.close() //nolint:errcheck // read-only verification handles
	for _, device := range expected {
		link, err := p.ownedLink(lease{Sandbox: id, Interface: device})
		if err != nil {
			return err
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("TAP %s is down", device.TAP)
		}
		key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface index
		var actual endpoint
		if err := d.endpoints.Lookup(key, &actual); err != nil {
			return err
		}
		want, err := p.endpoint(device, actual.Enabled != 0)
		if err != nil {
			return err
		}
		if want != actual {
			return fmt.Errorf("TAP %s has a stale eBPF endpoint", device.TAP)
		}
		for _, program := range []struct {
			parent uint32
			value  *ebpf.Program
		}{{netlink.HANDLE_MIN_INGRESS, d.ingress}, {netlink.HANDLE_MIN_EGRESS, d.egress}} {
			filters, err := netlink.FilterList(link, program.parent)
			if err != nil {
				return err
			}
			info, err := program.value.Info()
			if err != nil {
				return err
			}
			programID, _ := info.ID()
			found := false
			for _, filter := range filters {
				if bpf, ok := filter.(*netlink.BpfFilter); ok && bpf.Id == int(programID) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("TAP %s is missing its eBPF filter", device.TAP)
			}
		}
	}
	return nil
}

// Recover reconstructs host objects from durable endpoints while preserving
// guest-visible IP/MAC identity. Stale ifindices are never treated as ownership.
func (p *Provider) Recover(ctx context.Context, id types.SandboxID, name string, expected []types.NetworkInterface) ([]types.NetworkInterface, error) {
	specs := make([]network.AddSpec, len(expected))
	for index := range expected {
		specs[index] = network.AddSpec{Index: expected[index].Index, Queues: expected[index].Queues, Existing: &expected[index]}
	}
	return p.Add(ctx, id, name, specs...)
}

// Quiesce removes packet permission before retained host resources can be reused.
func (p *Provider) Quiesce(ctx context.Context, id types.SandboxID) error {
	return p.setEnabled(ctx, id, false)
}

// Unquiesce activates existing mappings before the guest resumes.
func (p *Provider) Unquiesce(ctx context.Context, id types.SandboxID) error {
	return p.setEnabled(ctx, id, true)
}

func (p *Provider) setEnabled(ctx context.Context, id types.SandboxID, enabled bool) error {
	items, err := p.indexedLeases(ctx, id)
	if err != nil || len(items) == 0 {
		return err
	}
	d, err := p.dataPlane(ctx)
	if err != nil {
		return err
	}
	defer d.close() //nolint:errcheck // scoped handles
	for _, item := range items {
		link, err := p.ownedLink(item)
		if err != nil {
			return err
		}
		value, err := p.endpoint(item.Interface, enabled)
		if err != nil {
			return err
		}
		key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface index is positive int32
		if err := d.endpoints.Update(key, value, ebpf.UpdateAny); err != nil {
			return err
		} //nolint:gosec // kernel interface index
	}
	return nil
}

// Remove tears down selected slots. It retains leases if kernel cleanup fails,
// preventing an old conntrack entry from reaching a later address owner.
func (p *Provider) Remove(ctx context.Context, id types.SandboxID, indices ...int) error {
	items, err := p.leases(ctx, id)
	if err != nil || len(items) == 0 {
		return err
	}
	d, err := p.dataPlane(ctx)
	if err != nil {
		return err
	}
	defer d.close() //nolint:errcheck // scoped handles
	for _, item := range items {
		selected := false
		for _, index := range indices {
			selected = selected || item.Interface.Index == index
		}
		if !selected {
			continue
		}
		link, err := p.ownedLink(item)
		var missing netlink.LinkNotFoundError
		if err != nil && !errors.As(err, &missing) {
			return err
		}
		if err == nil {
			key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface indices fit uint32
			if err := d.endpoints.Delete(key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				return err
			} //nolint:gosec // kernel interface index
			if err := netlink.LinkDel(link); err != nil {
				return err
			}
		}
		filter := &netlink.ConntrackFilter{}
		if err := filter.AddIP(netlink.ConntrackOrigSrcIP, net.ParseIP(item.Interface.HostAddress)); err != nil {
			return err
		}
		reverse := &netlink.ConntrackFilter{}
		if err := reverse.AddIP(netlink.ConntrackOrigDstIP, net.ParseIP(item.Interface.HostAddress)); err != nil {
			return err
		}
		if _, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, filter, reverse); err != nil {
			return fmt.Errorf("clear endpoint conntrack before address reuse: %w", err)
		}
		path := p.indexPath(id, item.Interface.Index)
		if err := os.Remove(path); err == nil {
			if err := syncDirectory(filepath.Dir(path)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := p.retireAddress(item); err != nil {
			return err
		}
	}
	return nil
}

// Delete is retryable after partial failure and never touches unrelated TAPs.
func (p *Provider) Delete(ctx context.Context, id types.SandboxID) error {
	indices, err := p.Allocated(ctx, id)
	if err != nil {
		return err
	}
	return p.Remove(ctx, id, indices...)
}

// Allocated includes durable reservations left by interrupted creation.
func (p *Provider) Allocated(ctx context.Context, id types.SandboxID) ([]int, error) {
	items, err := p.leases(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]int, 0, len(items))
	for _, item := range items {
		result = append(result, item.Interface.Index)
	}
	return result, nil
}

// IndexForTAP only accepts deterministic names from this installation.
func (p *Provider) IndexForTAP(id types.SandboxID, name string) (int, bool) {
	for index := range types.MaxSandboxNICs {
		if p.tapName(id, index) == name {
			return index, true
		}
	}
	return 0, false
}

// OwnedIDs exposes durable allocations to the existing maintenance service.
func (p *Provider) OwnedIDs(ctx context.Context) ([]types.SandboxID, error) {
	items, err := p.leases(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := make(map[types.SandboxID]bool)
	var result []types.SandboxID
	for _, item := range items {
		if !seen[item.Sandbox] {
			seen[item.Sandbox] = true
			result = append(result, item.Sandbox)
		}
	}
	return result, nil
}
