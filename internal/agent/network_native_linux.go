//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Native guests have no network daemon. Serialize complete replacements so
// concurrent protocol requests cannot interleave addresses, routes and DNS.
var nativeNetworkMu sync.Mutex

func applyNativeNetwork(ctx context.Context, configuration NetworkConfig) error {
	if err := configuration.Validate(); err != nil {
		return err
	}
	if hasDHCP(configuration.Interfaces) {
		return fmt.Errorf("native guest profile requires static network configuration")
	}
	nativeNetworkMu.Lock()
	defer nativeNetworkMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte(configuration.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := os.WriteFile("/etc/hostname", []byte(configuration.Hostname+"\n"), 0o644); err != nil { //nolint:gosec // fixed guest OS path
		return fmt.Errorf("write hostname: %w", err)
	}
	if !guestNetworkReady(configuration) {
		for index, device := range configuration.Interfaces {
			if err := ctx.Err(); err != nil {
				return err
			}
			link, err := linkByMAC(ctx, device.MAC)
			if err != nil {
				return err
			}
			if err := configureStaticLink(link, device, 100+index); err != nil {
				return fmt.Errorf("configure %s: %w", device.MAC, err)
			}
		}
	}
	if err := writeResolverAt("/", configuration.DNSServers); err != nil {
		return fmt.Errorf("configure resolver: %w", err)
	}
	return nil
}

// configureStaticLink removes captured IPv4 identity before installing the new
// address. Link-local addresses deliberately use global scope with a preferred
// default-route source, matching the fixed-identity host translator.
func configureStaticLink(link netlink.Link, device NetworkInterface, priority int) error {
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		ones, _ := address.Mask.Size()
		if address.IP.String() != device.Address || ones != device.Prefix || address.Scope != int(netlink.SCOPE_UNIVERSE) {
			if err := netlink.AddrDel(link, &address); err != nil {
				return err
			}
		}
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
			if err := netlink.RouteDel(&route); err != nil {
				return err
			}
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if device.Address == "" {
		return nil
	}
	address := &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(device.Address).To4(), Mask: net.CIDRMask(device.Prefix, 32)}}
	if err := netlink.AddrReplace(link, address); err != nil {
		return err
	}
	if device.Gateway == "" {
		return nil
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index, Gw: net.ParseIP(device.Gateway).To4(),
		Src: address.IP, Priority: priority,
	})
}
