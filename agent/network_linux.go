//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	networkReloadGrace = 300 * time.Millisecond
	networkDHCPGrace   = 5 * time.Second
	networkPollDelay   = 10 * time.Millisecond
)

// applyGuestNetwork owns the guest OS mechanism behind the wire protocol.
func applyGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	unchanged := networkFilesMatch("/etc/systemd/network", configuration)
	if !unchanged {
		if err := writeNetworkFiles("/etc/systemd/network", configuration); err != nil {
			return fmt.Errorf("write network files: %w", err)
		}
	}
	if err := os.WriteFile("/etc/hostname", []byte(configuration.Hostname+"\n"), 0o644); err != nil { //nolint:gosec // fixed guest OS path
		return fmt.Errorf("write hostname: %w", err)
	}
	if err := unix.Sethostname([]byte(configuration.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := writeFixedResolverAt("/", configuration); err != nil {
		return fmt.Errorf("configure fixed guest resolver: %w", err)
	}
	if len(configuration.Interfaces) == 0 {
		return nil
	}
	if unchanged && guestNetworkReady(configuration) {
		return nil
	}
	if err := prepareDHCPLinks(ctx, configuration.Interfaces); err != nil {
		return err
	}
	// Reload applies changed .network files to their matching links without
	// restarting the whole daemon. Verify the effective addresses and routes;
	// older or incompatible networkd builds retain the restart fallback.
	if err := reloadGuestNetwork(ctx, configuration); err == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "systemctl", "restart", "systemd-networkd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart systemd-networkd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return waitGuestNetwork(ctx, configuration)
}

// prepareDHCPLinks removes addresses left in captured guest memory before the
// new DHCP client runs. Without it a clone can keep advertising the source IP
// until the lease happens to be renewed.
func prepareDHCPLinks(ctx context.Context, interfaces []NetworkInterface) error {
	for _, device := range interfaces {
		if !device.DHCP {
			continue
		}
		link, err := linkByMAC(ctx, device.MAC)
		if err != nil {
			return fmt.Errorf("find DHCP NIC %s: %w", device.MAC, err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("list addresses on %s: %w", link.Attrs().Name, err)
		}
		for _, address := range addresses {
			if err := netlink.AddrDel(link, &address); err != nil {
				return fmt.Errorf("flush address %s on %s: %w", address.IPNet, link.Attrs().Name, err)
			}
		}
	}
	return nil
}

func linkByMAC(ctx context.Context, mac string) (netlink.Link, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(networkPollDelay)
	defer ticker.Stop()
	for {
		links, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		for _, candidate := range links {
			if strings.EqualFold(candidate.HardwareAddr.String(), mac) {
				return netlink.LinkByIndex(candidate.Index)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("no link with MAC %s", mac)
		case <-ticker.C:
		}
	}
}

func waitGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	timeout := networkReloadGrace
	if hasDHCP(configuration.Interfaces) {
		timeout = networkDHCPGrace
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(networkPollDelay)
	defer ticker.Stop()
	for {
		if guestNetworkReady(configuration) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("network did not converge after %s", timeout)
		case <-ticker.C:
		}
	}
}

func hasDHCP(interfaces []NetworkInterface) bool {
	for _, device := range interfaces {
		if device.DHCP {
			return true
		}
	}
	return false
}

func reloadGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	output, err := exec.CommandContext(ctx, "networkctl", "reload").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reload systemd-networkd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	// Reload already reconfigures links matching a changed .network file.
	// Another reconfigure can restart DHCP while that first request is pending.
	return waitGuestNetwork(ctx, configuration)
}

// guestNetworkReady checks the target links' kernel state, including stale
// source IPv4 addresses and default routes, before acknowledging a clone.
func guestNetworkReady(configuration NetworkConfig) bool {
	links, err := net.Interfaces()
	if err != nil {
		return false
	}
	byMAC := make(map[string]net.Interface, len(links))
	for _, link := range links {
		byMAC[strings.ToLower(link.HardwareAddr.String())] = link
	}
	for _, expected := range configuration.Interfaces {
		link, found := byMAC[strings.ToLower(expected.MAC)]
		if !found || link.Flags&net.FlagUp == 0 {
			return false
		}
		addresses, err := link.Addrs()
		if err != nil {
			return false
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: link.Index}, netlink.RT_FILTER_OIF)
		if err != nil {
			return false
		}
		if expected.DHCP {
			if !guestHasIPv4(addresses) || !guestHasDefaultRoute(routes) {
				return false
			}
			continue
		}
		if !guestIPv4Matches(addresses, expected.Address, expected.Prefix) || !guestDefaultRouteMatches(routes, expected.Gateway) {
			return false
		}
		if net.ParseIP(expected.Address).IsLinkLocalUnicast() && expected.Gateway != "" && !guestRouteSourceMatches(routes, expected.Address) {
			return false
		}
	}
	return true
}

func guestRouteSourceMatches(routes []netlink.Route, address string) bool {
	for _, route := range routes {
		if route.Dst != nil {
			ones, _ := route.Dst.Mask.Size()
			if ones != 0 {
				continue
			}
		}
		if route.Src.Equal(net.ParseIP(address)) {
			return true
		}
	}
	return false
}

func guestHasIPv4(addresses []net.Addr) bool {
	for _, value := range addresses {
		network, ok := value.(*net.IPNet)
		if ok && network.IP.To4() != nil && !network.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func guestHasDefaultRoute(routes []netlink.Route) bool {
	for _, route := range routes {
		if route.Dst == nil {
			return true
		}
		ones, _ := route.Dst.Mask.Size()
		if ones == 0 {
			return true
		}
	}
	return false
}

func guestIPv4Matches(addresses []net.Addr, address string, prefix int) bool {
	found := address == ""
	for _, value := range addresses {
		network, ok := value.(*net.IPNet)
		if !ok || network.IP.To4() == nil || (network.IP.IsLinkLocalUnicast() && network.IP.String() != address) {
			continue
		}
		bits, _ := network.Mask.Size()
		if network.IP.String() != address || bits != prefix {
			return false
		}
		found = true
	}
	return found
}

func guestDefaultRouteMatches(routes []netlink.Route, gateway string) bool {
	want := net.ParseIP(gateway)
	found := gateway == ""
	for _, route := range routes {
		if route.Dst != nil {
			ones, _ := route.Dst.Mask.Size()
			if ones != 0 {
				continue
			}
		}
		if want == nil || !route.Gw.Equal(want) {
			return false
		}
		found = true
	}
	return found
}
