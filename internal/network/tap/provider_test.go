package tap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

func testProvider(t *testing.T) *Provider {
	t.Helper()
	root := t.TempDir()
	p, err := New(Options{StateDir: filepath.Join(root, "state"), PinDir: filepath.Join(root, "pins"), Subnet: "10.89.0.0/24", TAPPrefix: "kt"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFixedIdentitySeparatesGuestAndHostAddresses(t *testing.T) {
	p := testProvider(t)
	var items []lease
	for range 2 {
		id, err := types.NewSandboxID()
		if err != nil {
			t.Fatal(err)
		}
		item, err := p.reserve(t.Context(), id, network.AddSpec{Index: 0})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if items[0].Interface.MAC != items[1].Interface.MAC || *items[0].Interface.IPv4 != *items[1].Interface.IPv4 ||
		items[0].Interface.HostAddress == items[1].Interface.HostAddress || items[0].Interface.TAP == items[1].Interface.TAP {
		t.Fatalf("guest identity must match while host ownership remains distinct: %+v", items)
	}
	mac, ipv4, err := guestIdentity(1)
	if err != nil || mac == items[0].Interface.MAC || ipv4.Address == items[0].Interface.IPv4.Address || ipv4.Gateway != "" {
		t.Fatalf("secondary NIC: %s %+v %v", mac, ipv4, err)
	}
}

func TestConcurrentReservationsNeverReuseHostAddress(t *testing.T) {
	p := testProvider(t)
	const count = 32
	results := make(chan lease, count)
	errors := make(chan error, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := types.NewSandboxID()
			if err != nil {
				errors <- err
				return
			}
			item, err := p.reserve(t.Context(), id, network.AddSpec{})
			if err != nil {
				errors <- err
				return
			}
			results <- item
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	seen := make(map[string]bool)
	for item := range results {
		if seen[item.Interface.HostAddress] {
			t.Fatalf("duplicate %s", item.Interface.HostAddress)
		}
		seen[item.Interface.HostAddress] = true
	}
	if len(seen) != count {
		t.Fatalf("reserved %d addresses, want %d", len(seen), count)
	}
}

func TestReservationRecoversInterruptedIndexPublication(t *testing.T) {
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	first, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.indexPath(id, 0)); err != nil {
		t.Fatal(err)
	}
	recovered, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil || recovered.Interface.HostAddress != first.Interface.HostAddress {
		t.Fatalf("recovery changed reservation: %+v %v", recovered, err)
	}
	items, err := p.leases(t.Context(), id)
	if err != nil || len(items) != 1 {
		t.Fatalf("lost allocation inventory: %+v %v", items, err)
	}
	indexed, err := p.indexedLeases(t.Context(), id)
	if err != nil || len(indexed) != 1 || indexed[0].Interface.HostAddress != first.Interface.HostAddress {
		t.Fatalf("indexed recovery: %+v %v", indexed, err)
	}
}

func TestReservationRejectsCancelledAndDHCPRequests(t *testing.T) {
	p := testProvider(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	id, _ := types.NewSandboxID()
	if _, err := p.reserve(ctx, id, network.AddSpec{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled allocation: %v", err)
	}
	if _, err := p.reserve(t.Context(), id, network.AddSpec{DHCP: true}); err == nil {
		t.Fatal("DHCP accepted")
	}
}

func TestReleasedAddressWaitsBeforeReuse(t *testing.T) {
	p := testProvider(t)
	clock := time.Now().UTC()
	p.now = func() time.Time { return clock }
	id, err := types.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	retire := func(item lease) {
		t.Helper()
		if err := os.Remove(p.indexPath(id, 0)); err != nil {
			t.Fatal(err)
		}
		if err := p.retireAddress(item); err != nil {
			t.Fatal(err)
		}
	}
	retire(first)
	active, err := p.leases(t.Context(), id)
	if err != nil || len(active) != 0 {
		t.Fatalf("retired lease remained active: %+v %v", active, err)
	}
	second, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Interface.HostAddress == first.Interface.HostAddress {
		t.Fatal("address reused during TCP quarantine")
	}
	retire(second)
	clock = clock.Add(p.options.ReuseDelay + time.Second)
	reused, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if reused.Interface.HostAddress != first.Interface.HostAddress || !reused.ReuseAfter.IsZero() {
		t.Fatalf("expired address did not become available: %+v", reused)
	}
}

func TestAddressPoolReportsExhaustion(t *testing.T) {
	options := testProvider(t).options
	options.Subnet = "10.89.0.0/28"
	p, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	for range 14 {
		id, err := types.NewSandboxID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.reserve(t.Context(), id, network.AddSpec{}); err != nil {
			t.Fatal(err)
		}
	}
	id, err := types.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.reserve(t.Context(), id, network.AddSpec{}); err == nil {
		t.Fatal("address pool overcommitted")
	}
}

func TestHostTAPNamesIncludeCompleteSandboxAndInstallationIdentity(t *testing.T) {
	first := testProvider(t)
	second := testProvider(t)
	one := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	two := types.SandboxID("123e4567-e89b-42d3-a456-426614174001")
	a, b, c := first.tapName(one, 0), first.tapName(two, 0), second.tapName(one, 0)
	if a == b || a == c || len(a) > 15 || len(b) > 15 || len(c) > 15 {
		t.Fatalf("host TAP collision: %s %s %s", a, b, c)
	}
}
