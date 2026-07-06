//go:build linux

package network

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

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
	if rec.MAC != "" {
		mac, err := net.ParseMAC(rec.MAC)
		if err != nil {
			return nil, false, fmt.Errorf("parse tap MAC: %w", err)
		}
		attrs.HardwareAddr = mac
	}
	tap := &netlink.Tuntap{
		LinkAttrs: attrs,
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_DEFAULTS,
	}
	if rec.NumQueues > 1 {
		tap.Queues = rec.NumQueues
		tap.Flags = netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, false, fmt.Errorf("create tap %s: %w", rec.TAP, err)
	}
	link, err := netlink.LinkByName(rec.TAP)
	if err != nil {
		_ = netlink.LinkDel(tap)
		return nil, false, fmt.Errorf("find created tap %s: %w", rec.TAP, err)
	}
	return link, true, nil
}
