//go:build linux

package agent

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestNativeNetworkRejectsDHCPBeforeChangingGuest(t *testing.T) {
	configuration := NetworkConfig{Hostname: "guest", Interfaces: []NetworkInterface{{MAC: "02:00:00:00:00:01", DHCP: true}}}
	if err := applyNativeNetwork(context.Background(), configuration); err == nil {
		t.Fatal("native profile accepted DHCP without a DHCP client")
	}
}

func TestNativeNetworkHonorsCancellationBeforeChangingGuest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := applyNativeNetwork(ctx, NetworkConfig{Hostname: "guest"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled configuration: %v", err)
	}
}

func TestGuestIPv4MatchesRejectsStaleSourceAddress(t *testing.T) {
	addresses := []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.88.0.7"), Mask: net.CIDRMask(16, 32)},
		&net.IPNet{IP: net.ParseIP("10.88.0.8"), Mask: net.CIDRMask(16, 32)},
	}
	if guestIPv4Matches(addresses, "10.88.0.8", 16) {
		t.Fatal("clone accepted a stale source IPv4 address")
	}
	if !guestIPv4Matches(addresses[1:], "10.88.0.8", 16) {
		t.Fatal("clone rejected its only assigned IPv4 address")
	}
}

func TestGuestIPv4MatchesFixedLinkLocalAddress(t *testing.T) {
	addresses := []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.240.2"), Mask: net.CIDRMask(30, 32)}}
	if !guestIPv4Matches(addresses, "169.254.240.2", 30) {
		t.Fatal("fixed link-local guest identity was ignored")
	}
}

func TestGuestDefaultRouteMatches(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.88.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	routes := []netlink.Route{{Dst: subnet, Gw: net.ParseIP("10.88.0.1")}}
	if guestDefaultRouteMatches(routes, "10.88.0.1") {
		t.Fatal("subnet route was accepted as default")
	}
	routes = append(routes, netlink.Route{Gw: net.ParseIP("10.88.0.1")})
	if !guestDefaultRouteMatches(routes, "10.88.0.1") {
		t.Fatal("default route was not found")
	}
	if guestDefaultRouteMatches(routes, "") {
		t.Fatal("unexpected default route was accepted on a link without gateway")
	}
	routes = append(routes, netlink.Route{Gw: net.ParseIP("10.88.0.254")})
	if guestDefaultRouteMatches(routes, "10.88.0.1") {
		t.Fatal("stale default route was accepted")
	}
	routes = []netlink.Route{{Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, Gw: net.ParseIP("10.88.0.1")}}
	if !guestDefaultRouteMatches(routes, "10.88.0.1") {
		t.Fatal("explicit 0/0 route was not recognized")
	}
}
