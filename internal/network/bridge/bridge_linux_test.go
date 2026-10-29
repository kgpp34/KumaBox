//go:build linux

package bridge

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

func TestDHCPReservationDoesNotWaitForStaticIPAM(t *testing.T) {
	provider := &Provider{prefix: "bt", queueSize: 512, leaseDir: t.TempDir()}
	guard, err := provider.lockLeases(t.Context(), "br0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Unlock(context.Background()); err != nil {
			t.Error(err)
		}
	})
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	addresses, err := provider.reserveAddresses(ctx, id, &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: "br0"},
	}, []network.AddSpec{{Index: 0, DHCP: true}})
	if err != nil {
		t.Fatalf("DHCP reservation blocked behind static IPAM: %v", err)
	}
	if len(addresses) != 1 || addresses[0] != nil {
		t.Fatalf("DHCP allocated a host-side static address: %v", addresses)
	}
	owned, err := provider.OwnedIDs(t.Context())
	if err != nil || len(owned) != 1 || owned[0] != id {
		t.Fatalf("DHCP ownership was not retained: %v, %v", owned, err)
	}
}

func TestBridgeLeaseLockHonorsCancellation(t *testing.T) {
	provider := &Provider{leaseDir: t.TempDir()}
	guard, err := provider.lockLeases(t.Context(), "br0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Unlock(context.Background()); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := provider.lockLeases(ctx, "br0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended bridge lock = %v, want deadline exceeded", err)
	}
}

func TestBridgeLeaseAllocationAndRelease(t *testing.T) {
	provider := &Provider{prefix: "bt", queueSize: 512, leaseDir: t.TempDir()}
	subnet := &net.IPNet{IP: net.ParseIP("10.99.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	gateway := net.ParseIP("10.99.0.1")
	first := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	second := types.SandboxID("223e4567-e89b-42d3-a456-426614174000")

	address, err := provider.allocateAddress("kubebr0", subnet, gateway, first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.99.0.2" {
		t.Fatalf("first address = %s, want 10.99.0.2", address)
	}
	if err := provider.claimLease("kubebr0", address, first, 0); err != nil {
		t.Fatalf("same owner could not claim its lease: %v", err)
	}
	if err := provider.claimLease("kubebr0", address, second, 0); err == nil {
		t.Fatal("another sandbox claimed the same address")
	}
	other, err := provider.allocateAddress("kubebr0", subnet, gateway, second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if other == address {
		t.Fatalf("allocator reused %s", address)
	}
	if err := provider.releaseLeases(first); err != nil {
		t.Fatal(err)
	}
	reused, err := provider.allocateAddress("kubebr0", subnet, gateway, second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reused != address {
		t.Fatalf("released address was not reused: got %s, want %s", reused, address)
	}
}

func TestBridgeOwnedIDs(t *testing.T) {
	provider := &Provider{prefix: "bt", queueSize: 512, leaseDir: t.TempDir()}
	subnet := &net.IPNet{IP: net.ParseIP("10.99.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	gateway := net.ParseIP("10.99.0.1")
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	if _, err := provider.allocateAddress("kubebr0", subnet, gateway, id, 0); err != nil {
		t.Fatal(err)
	}
	owned, err := provider.OwnedIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0] != id {
		t.Fatalf("OwnedIDs = %v", owned)
	}
}

func TestBridgeIndexForTAP(t *testing.T) {
	provider := &Provider{prefix: "bt", queueSize: 512, leaseDir: t.TempDir()}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	tap, err := network.TAPName(provider.prefix, id, 3)
	if err != nil {
		t.Fatal(err)
	}
	if index, ok := provider.IndexForTAP(id, tap); !ok || index != 3 {
		t.Fatalf("IndexForTAP = %d, %t", index, ok)
	}
	if _, ok := provider.IndexForTAP(id, "bt00000000-0"); ok {
		t.Fatal("foreign TAP was accepted")
	}
}

func TestBridgeAllocatedIncludesStaticLease(t *testing.T) {
	provider := &Provider{prefix: "bt", queueSize: 512, leaseDir: t.TempDir()}
	subnet := &net.IPNet{IP: net.ParseIP("10.99.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	if _, err := provider.allocateAddress("kubebr0", subnet, net.ParseIP("10.99.0.1"), id, 2); err != nil {
		t.Fatal(err)
	}
	allocated, err := provider.Allocated(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocated) != 1 || allocated[0] != 2 {
		t.Fatalf("Allocated = %v", allocated)
	}
}
