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
	networkPollDelay   = 10 * time.Millisecond
)

// applyGuestNetwork owns the guest OS mechanism behind the wire protocol.
func applyGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	if err := writeNetworkFiles("/etc/systemd/network", configuration); err != nil {
		return fmt.Errorf("write network files: %w", err)
	}
	if err := os.WriteFile("/etc/hostname", []byte(configuration.Hostname+"\n"), 0o644); err != nil { //nolint:gosec // fixed guest OS path
		return fmt.Errorf("write hostname: %w", err)
	}
	if err := unix.Sethostname([]byte(configuration.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if len(configuration.Interfaces) == 0 {
		return nil
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
	return nil
}

func reloadGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	output, err := exec.CommandContext(ctx, "networkctl", "reload").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reload systemd-networkd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	deadline := time.NewTimer(networkReloadGrace)
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
			return fmt.Errorf("network did not converge after reload")
		case <-ticker.C:
		}
	}
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
		if err != nil || !guestIPv4Matches(addresses, expected.Address, expected.Prefix) {
			return false
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: link.Index}, netlink.RT_FILTER_OIF)
		if err != nil || !guestDefaultRouteMatches(routes, expected.Gateway) {
			return false
		}
	}
	return true
}

func guestIPv4Matches(addresses []net.Addr, address string, prefix int) bool {
	found := address == ""
	for _, value := range addresses {
		network, ok := value.(*net.IPNet)
		if !ok || network.IP.To4() == nil || network.IP.IsLinkLocalUnicast() {
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
