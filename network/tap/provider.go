// Package tap connects independent host TAPs using fixed guest identities and
// eBPF address translation. Guests share template-local IP/MAC values; each TAP
// has a distinct routed host address, without a shared Ethernet bridge.
//
//	guest (fixed IP) -> TAP ingress BPF -> unique host IP -> routing/conntrack
//	guest (fixed IP) <- TAP egress BPF  <- unique host IP <- routing/conntrack
//
// Linux supplies external masquerading in this first data plane. Host setup is
// installation-wide; adding a sandbox never installs a per-sandbox firewall rule.
package tap

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kumabox/kumabox/types"
)

// Options configures one installation. Persistent state and bpffs pins are
// separate: host reboot discards BPF state, while address leases survive it.
type Options struct {
	StateDir  string // StateDir owns durable address reservations and TAP identities.
	PinDir    string // PinDir is a private directory beneath the host bpffs mount.
	Subnet    string // Subnet contains unique host endpoint addresses, never guest IPs.
	TAPPrefix string // TAPPrefix separates installations in the host namespace.
	// ReuseDelay quarantines released host addresses past the TCP TIME_WAIT
	// interval. Zero selects 90 seconds; it never delays ordinary allocation.
	ReuseDelay time.Duration
}

// Provider owns TAPs and their eBPF mappings under one runtime's state ownership.
// Kernel handles are scoped to operations. Recovery retains only addresses that
// may lack a sandbox index; ordinary queries read the durable index directly.
type Provider struct {
	options  Options
	pool     netip.Prefix
	owner    string
	now      func() time.Time
	recovery leaseRecovery
}

// New validates configuration without modifying the host or loading eBPF.
func New(options Options) (*Provider, error) {
	if options.ReuseDelay < 0 {
		return nil, errors.New("address reuse delay must not be negative")
	}
	if options.ReuseDelay == 0 {
		options.ReuseDelay = 90 * time.Second
	}
	pool, err := netip.ParsePrefix(options.Subnet)
	if err != nil || !pool.Addr().Is4() || !pool.Addr().IsPrivate() || pool != pool.Masked() || pool.Bits() < 16 || pool.Bits() > 28 {
		return nil, errors.New("eBPF subnet must be a canonical private IPv4 /16 through /28")
	}
	if !filepath.IsAbs(options.StateDir) || !filepath.IsAbs(options.PinDir) || options.StateDir == "/" || options.PinDir == "/sys/fs/bpf" {
		return nil, errors.New("eBPF state and pin directories must be private absolute paths")
	}
	if options.TAPPrefix == "" || len(options.TAPPrefix) > 5 {
		return nil, errors.New("eBPF TAP prefix must contain 1..5 ASCII letters or digits")
	}
	for _, ch := range options.TAPPrefix {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return nil, errors.New("eBPF TAP prefix must contain ASCII letters or digits")
		}
	}
	digest := sha256.Sum256([]byte(filepath.Clean(options.StateDir)))
	return &Provider{
		options: options, pool: pool, owner: hex.EncodeToString(digest[:8]), now: time.Now,
		recovery: leaseRecovery{gate: make(chan struct{}, 1)},
	}, nil
}

// Type identifies the host data plane independently of guest address policy.
func (*Provider) Type() types.NetworkBackend { return types.NetworkBackendEBPF }

// tapName hashes the complete root/VM identity into the Linux name budget.
// Host TAPs share one namespace, so truncating UUIDs to eight hex digits would
// produce avoidable collisions at high density. The alias retains the full ID.
func (p *Provider) tapName(id types.SandboxID, index int) string {
	suffix := "-" + strconv.FormatInt(int64(index), 36)
	digest := sha256.Sum256([]byte(p.owner + ":" + id.String()))
	identity := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
	return p.options.TAPPrefix + identity[:15-len(p.options.TAPPrefix)-len(suffix)] + suffix
}

// guestIdentity is stable per NIC slot across every clone of a template. Each
// NIC uses its own /30; only eth0 installs a default route.
func guestIdentity(index int) (string, *types.IPv4Config, error) {
	if index < 0 || index >= types.MaxSandboxNICs {
		return "", nil, fmt.Errorf("invalid fixed guest NIC slot %d", index)
	}
	base := index * 4
	gateway := ""
	if index == 0 {
		gateway = fmt.Sprintf("169.254.240.%d", base+1)
	}
	return fmt.Sprintf("02:4b:42:00:%02x:02", index), &types.IPv4Config{
		Address: fmt.Sprintf("169.254.240.%d", base+2), Gateway: gateway, Prefix: 30,
	}, nil
}
