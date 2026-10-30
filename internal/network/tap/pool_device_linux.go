//go:build linux

package tap

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

// preparePoolLink installs the expensive device-wide state once, with no
// endpoint permission. Idle TAPs hold no queue FDs: monitors open by name, and
// a single-queue TAP cannot have a second userspace owner.
func (p *Provider) preparePoolLink(entry *pooledTAP, d *datapath) error {
	link, err := netlink.LinkByName(entry.name)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		link, err = createTAP(entry.name, p.poolAlias(entry.shape, entry.epoch), entry.shape.queues)
	}
	if err != nil {
		return err
	}
	shape, epoch, owned := p.parsePoolAlias(link.Attrs().Alias)
	if !owned || shape != entry.shape || epoch > entry.epoch {
		return errors.New("refuse unrelated or newer pooled TAP")
	}
	if entry.owner == "" || epoch != entry.epoch {
		if err := requireIdleQueues(link); err != nil {
			return err
		}
	}
	key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface index
	if err := d.endpoints.Delete(key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	if err := attachFilters(link, d); err != nil {
		return err
	}
	mac, ip, err := guestIdentity(entry.shape.slot)
	if err != nil {
		return err
	}
	value, err := p.endpoint(types.NetworkInterface{
		Index: entry.shape.slot, Name: fmt.Sprint("eth", entry.shape.slot), TAP: entry.name,
		MAC: mac, IPv4: ip, Queues: entry.shape.queues, QueueSize: network.DefaultQueueSize,
		Network: "ebpf", AddressMode: types.AddressModeFixed, HostAddress: p.pool.Addr().Next().String(),
	}, false)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetHardwareAddr(link, net.HardwareAddr(value.HostMAC[:])); err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(value.Gateway[:]), Mask: net.CIDRMask(32, 32)}}); err != nil {
		return err
	}
	if err := netlink.LinkSetAlias(link, p.poolAlias(entry.shape, entry.epoch)); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	p.taps.mu.Lock()
	entry.prepared = true
	p.taps.mu.Unlock()
	return nil
}

func requireIdleQueues(link netlink.Link) error {
	tap, ok := link.(*netlink.Tuntap)
	if !ok || tap.Mode != netlink.TUNTAP_MODE_TAP {
		return errors.New("pooled interface is not a TAP")
	}
	if tap.Queues != 0 || tap.DisabledQueues != 0 {
		return fmt.Errorf("TAP %s still has attached queue descriptors", tap.Name)
	}
	if tap.Flags&netlink.TUNTAP_MULTI_QUEUE == 0 {
		// Linux exports queue counters for multi-queue TAPs only. A single
		// queue must be tested by TUNSETIFF: EBUSY proves an existing holder.
		// Do not mistake its absent netlink counters for zero references.
		fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		request, err := unix.NewIfreq(tap.Name)
		if err == nil {
			request.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
			err = unix.IoctlIfreq(fd, unix.TUNSETIFF, request)
		}
		if err := errors.Join(err, unix.Close(fd)); err != nil {
			return fmt.Errorf("prove exclusive TAP queue ownership: %w", err)
		}
	}
	return nil
}

func (p *Provider) ensurePooledTAP(item lease, d *datapath) error {
	if err := p.adoptPooledLease(item); err != nil {
		return err
	}
	p.taps.mu.Lock()
	entry := p.taps.entries[item.Interface.TAP]
	prepared := entry.prepared
	p.taps.mu.Unlock()
	if !prepared {
		if err := p.preparePoolLink(entry, d); err != nil {
			return err
		}
	}
	link, err := netlink.LinkByName(entry.name)
	if err != nil {
		return err
	}
	shape, epoch, owned := p.parsePoolAlias(link.Attrs().Alias)
	if !owned || shape != entry.shape || epoch > item.Epoch {
		return errors.New("pooled TAP ownership changed")
	}
	if epoch != item.Epoch {
		if err := requireIdleQueues(link); err != nil {
			return err
		}
		if err := netlink.LinkSetAlias(link, p.poolAlias(shape, item.Epoch)); err != nil {
			return err
		}
	}
	if err := p.publishEndpoint(item, link, d); err != nil {
		return err
	}
	p.taps.mu.Lock()
	entry.phase = poolActive
	p.taps.mu.Unlock()
	return nil
}

func (p *Provider) readyPoolLink(entry *pooledTAP, d *datapath) error {
	if !entry.prepared {
		return p.preparePoolLink(entry, d)
	}
	link, err := netlink.LinkByName(entry.name)
	if err != nil {
		return err
	}
	if err := requireIdleQueues(link); err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

// cleanPoolLink removes packet permission before inspecting queue ownership.
// Linux disposes a TAP queue's buffered frames when its last FD closes. We
// additionally take the link down; no VM or detached queue may still own it.
func (p *Provider) cleanPoolLink(entry *pooledTAP, d *datapath, host string) error {
	link, err := netlink.LinkByName(entry.name)
	if err != nil {
		return err
	}
	shape, epoch, owned := p.parsePoolAlias(link.Attrs().Alias)
	if !owned || shape != entry.shape || epoch > entry.epoch {
		return errors.New("refuse cleanup of unrelated or newer pooled TAP")
	}
	key := uint32(link.Attrs().Index) //nolint:gosec // kernel interface index
	if err := d.endpoints.Delete(key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	if err := requireIdleQueues(link); err != nil {
		return err
	}
	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_OIF)
	if err != nil {
		return err
	}
	cleared := false
	for _, route := range routes {
		if route.Dst == nil {
			continue
		}
		address, ok := netip.AddrFromSlice(route.Dst.IP)
		bits, _ := route.Dst.Mask.Size()
		if !ok || bits != 32 || !p.pool.Contains(address.Unmap()) {
			continue
		}
		if err := clearConntrack(route.Dst.IP.String()); err != nil {
			return err
		}
		cleared = cleared || route.Dst.IP.String() == host
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	if host != "" && !cleared {
		if err := clearConntrack(host); err != nil {
			return err
		}
	}
	neighbors, err := netlink.NeighList(link.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, neighbor := range neighbors {
		if err := netlink.NeighDel(&neighbor); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return netlink.LinkSetAlias(link, p.poolAlias(entry.shape, entry.epoch))
}
