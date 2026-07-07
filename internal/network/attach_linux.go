//go:build linux

package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// AttachHostTap creates a TAP device and enslaves it to the configured bridge.
//
// The TAP is created with IFF_NO_PI and vnet_hdr support because Cloud
// Hypervisor's virtio-net path expects packet frames without Linux's extra
// packet-info header and benefits from virtio network header offload metadata.
func AttachHostTap(rec Record) error {
	if rec.TAP == "" {
		return fmt.Errorf("tap name must not be empty")
	}
	if rec.BridgeDev == "" {
		return fmt.Errorf("bridge device must not be empty")
	}
	bridge, err := netlink.LinkByName(rec.BridgeDev)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", rec.BridgeDev, err)
	}
	tap, created, err := ensureTap(rec)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		if created {
			_ = netlink.LinkDel(tap)
		}
		return fmt.Errorf("attach tap %s to %s: %w", rec.TAP, rec.BridgeDev, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		if created {
			_ = netlink.LinkDel(tap)
		}
		return fmt.Errorf("set tap %s up: %w", rec.TAP, err)
	}
	return nil
}

// DeleteHostTap removes a per-VM TAP device.
//
// The operation is idempotent. VM delete and failure rollback both call it, and
// a missing device means the desired cleanup state has already been reached.
func DeleteHostTap(tapName string) error {
	if tapName == "" {
		return nil
	}
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

func ensureTap(rec Record) (netlink.Link, bool, error) {
	if link, err := netlink.LinkByName(rec.TAP); err == nil {
		return link, false, nil
	} else if !isLinkNotFound(err) {
		return nil, false, err
	}
	attrs := netlink.LinkAttrs{Name: rec.TAP}
	tap := &netlink.Tuntap{
		LinkAttrs: attrs,
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR,
	}
	if queuePairs := tapQueuePairs(rec.NumQueues); queuePairs > 1 {
		tap.Queues = queuePairs
		tap.Flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, false, fmt.Errorf("create tap %s: %w", rec.TAP, err)
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	link, err := netlink.LinkByName(rec.TAP)
	if err != nil {
		_ = netlink.LinkDel(tap)
		return nil, false, fmt.Errorf("find created tap %s: %w", rec.TAP, err)
	}
	return link, true, nil
}

func tapQueuePairs(numQueues int) int {
	if numQueues <= 2 {
		return 1
	}
	return numQueues / 2
}
