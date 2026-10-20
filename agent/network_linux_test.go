//go:build linux

package agent

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

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
