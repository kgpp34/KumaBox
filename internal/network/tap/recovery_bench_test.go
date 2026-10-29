package tap

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

// BenchmarkSandboxLeaseLookup includes durable ownership checks against real
// files. Historical retired addresses must not make one live NIC more costly.
// Fixture preparation and startup recovery are outside the measured loop.
func BenchmarkSandboxLeaseLookup(b *testing.B) {
	for _, history := range []int{0, 1000, 5000} {
		b.Run(fmt.Sprintf("history=%d", history), func(b *testing.B) {
			root := b.TempDir()
			p, err := New(Options{StateDir: filepath.Join(root, "state"), PinDir: filepath.Join(root, "pins"), Subnet: "10.89.0.0/16", TAPPrefix: "kt"})
			if err != nil {
				b.Fatal(err)
			}
			if err := os.MkdirAll(p.addressDir(), 0o700); err != nil {
				b.Fatal(err)
			}
			mac, ipv4, err := guestIdentity(0)
			if err != nil {
				b.Fatal(err)
			}
			address := netip.MustParseAddr("10.89.1.1")
			for number := range history {
				id := types.SandboxID(fmt.Sprintf("%08x-e89b-42d3-a456-426614174000", number+1))
				item := lease{Sandbox: id, ReuseAfter: time.Now().Add(time.Hour), Interface: types.NetworkInterface{
					Index: 0, Name: "eth0", TAP: p.tapName(id, 0), MAC: mac, IPv4: ipv4,
					Queues: 2, QueueSize: network.DefaultQueueSize, Network: "ebpf", AddressMode: types.AddressModeFixed,
					HostAddress: address.String(),
				}}
				raw, err := json.Marshal(item)
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.addressDir(), address.String()+".json"), raw, 0o600); err != nil {
					b.Fatal(err)
				}
				address = address.Next()
			}
			id, err := types.NewSandboxID()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := p.reserve(b.Context(), id, network.AddSpec{}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				items, err := p.leases(b.Context(), id)
				if err != nil || len(items) != 1 {
					b.Fatalf("lookup: %d leases, %v", len(items), err)
				}
			}
		})
	}
}
