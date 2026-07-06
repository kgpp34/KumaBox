package network

import (
	"errors"
	"net"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
)

func TestAllocatorAllocatesTapMACAndIPLease(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()

	allocation, err := NewAllocator(dir, cfg).Allocate(AllocateRequest{
		VMID:  "kb_allocator",
		Index: 0,
		CPU:   2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(allocation.Record.TAP) > maxInterfaceNameLength {
		t.Fatalf("tap length = %d", len(allocation.Record.TAP))
	}
	if allocation.Record.TAP != TapName(cfg.TapPrefix, "kb_allocator", 0) {
		t.Fatalf("tap = %q", allocation.Record.TAP)
	}
	mac, err := net.ParseMAC(allocation.Record.MAC)
	if err != nil {
		t.Fatal(err)
	}
	if mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Fatalf("MAC is not locally administered unicast: %s", allocation.Record.MAC)
	}
	if allocation.Config.Network == nil || allocation.Config.Network.IP != "10.88.0.2" {
		t.Fatalf("network config = %+v", allocation.Config.Network)
	}
	if allocation.Config.NumQueues != 4 {
		t.Fatalf("num queues = %d", allocation.Config.NumQueues)
	}

	leases, err := NewStore(dir).ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	lease, ok := leases["10.88.0.2"]
	if !ok {
		t.Fatalf("missing lease: %+v", leases)
	}
	if lease.VMID != "kb_allocator" || lease.MAC != allocation.Record.MAC || lease.TAP != allocation.Record.TAP {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestAllocatorSkipsUsedLeaseAndGateway(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	allocator := NewAllocator(dir, cfg)
	first, err := allocator.Allocate(AllocateRequest{VMID: "kb_first", Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocator.Allocate(AllocateRequest{VMID: "kb_second", Index: 0})
	if err != nil {
		t.Fatal(err)
	}

	if first.Config.Network.IP != "10.88.0.2" {
		t.Fatalf("first IP = %s", first.Config.Network.IP)
	}
	if second.Config.Network.IP != "10.88.0.3" {
		t.Fatalf("second IP = %s", second.Config.Network.IP)
	}
}

func TestAllocatorUsesCloudHypervisorMinimumNetworkQueues(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	allocation, err := NewAllocator(dir, cfg).Allocate(AllocateRequest{
		VMID:  "kb_one_cpu",
		Index: 0,
		CPU:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Config.NumQueues != 2 {
		t.Fatalf("num queues = %d, want 2", allocation.Config.NumQueues)
	}
}

func TestAllocatorRecoverExistingNetworkConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	existing := &Config{
		TAP:       "kbexist0",
		MAC:       "02:00:00:00:00:aa",
		NumQueues: 1,
		QueueSize: defaultQueueSize,
		Backend:   ProviderHostTap,
		Network: &GuestInfo{
			IP:      "10.88.0.9",
			Gateway: "10.88.0.1",
			Prefix:  16,
		},
	}

	allocation, err := NewAllocator(dir, cfg).Allocate(AllocateRequest{
		VMID:     "kb_recover",
		Index:    0,
		Existing: existing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Record.TAP != existing.TAP {
		t.Fatalf("tap = %s", allocation.Record.TAP)
	}
	if allocation.Record.MAC != existing.MAC {
		t.Fatalf("mac = %s", allocation.Record.MAC)
	}
	if allocation.Config.Network.IP != existing.Network.IP {
		t.Fatalf("ip = %s", allocation.Config.Network.IP)
	}

	leases, err := NewStore(dir).ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if leases["10.88.0.9"].VMID != "kb_recover" {
		t.Fatalf("leases = %+v", leases)
	}
}

func TestAllocatorRecoverExistingIPConflict(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	allocator := NewAllocator(dir, cfg)
	if _, err := allocator.Allocate(AllocateRequest{VMID: "kb_owner", Index: 0}); err != nil {
		t.Fatal(err)
	}

	_, err := allocator.Allocate(AllocateRequest{
		VMID:  "kb_conflict",
		Index: 0,
		Existing: &Config{
			TAP: "kbconflict0",
			MAC: "02:00:00:00:00:bb",
			Network: &GuestInfo{
				IP:     "10.88.0.2",
				Prefix: 16,
			},
		},
	})
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("err = %v, want ErrLeaseConflict", err)
	}
}

func TestAllocatorRecoverExistingMACConflict(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	allocator := NewAllocator(dir, cfg)
	owner, err := allocator.Allocate(AllocateRequest{VMID: "kb_owner", Index: 0})
	if err != nil {
		t.Fatal(err)
	}

	_, err = allocator.Allocate(AllocateRequest{
		VMID:  "kb_conflict",
		Index: 0,
		Existing: &Config{
			TAP: "kbconflict1",
			MAC: owner.Config.MAC,
			Network: &GuestInfo{
				IP:     "10.88.0.9",
				Prefix: 16,
			},
		},
	})
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("err = %v, want ErrLeaseConflict", err)
	}
}

func TestReleaseIPRemovesLease(t *testing.T) {
	dir := t.TempDir()
	cfg := testNetworkConfig()
	allocator := NewAllocator(dir, cfg)
	allocation, err := allocator.Allocate(AllocateRequest{VMID: "kb_release", Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := allocator.ReleaseIP(allocation.Config.Network.IP); err != nil {
		t.Fatal(err)
	}
	leases, err := NewStore(dir).ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases = %+v", leases)
	}
}

func testNetworkConfig() config.NetworkConfig {
	cfg := config.Default().Network
	cfg.CIDR = "10.88.0.0/16"
	cfg.Gateway = "10.88.0.1"
	cfg.TapPrefix = "kbtap"
	cfg.DNS = []string{"1.1.1.1"}
	return cfg
}
