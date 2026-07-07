package network

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/config"
)

const defaultQueueSize = 256

var ErrLeaseConflict = errors.New("network lease conflict")

// Allocator assigns deterministic tap names and exclusive MAC/IP leases.
//
// Allocation is file-backed and protected by the network lease lock so separate
// CLI processes cannot hand out the same guest IP concurrently.
type Allocator struct {
	store *Store
	cfg   config.NetworkConfig
}

// AllocateRequest describes one VM interface allocation.
//
// Existing is used during recovery/reconciliation to re-adopt a previously
// stored VM network config instead of assigning a new identity.
type AllocateRequest struct {
	VMID     string
	Network  string
	Index    int
	CPU      int
	Existing *Config
}

// Allocation contains both sides of a network assignment.
//
// Record is persisted in the provider index; Config is copied into the VM
// record and rendered into Cloud Hypervisor arguments.
type Allocation struct {
	Record Record `json:"record"`
	Config Config `json:"config"`
}

// NewAllocator returns an allocator backed by rootDir's network store.
func NewAllocator(rootDir string, cfg config.NetworkConfig) *Allocator {
	return &Allocator{
		store: NewStore(rootDir),
		cfg:   cfg,
	}
}

// Allocate reserves a tap/MAC/IP tuple for one VM interface.
//
// The IP lease is written before the caller creates the tap or provider record.
// Callers must ReleaseIP if later setup steps fail.
func (a *Allocator) Allocate(req AllocateRequest) (*Allocation, error) {
	if err := validateAllocateRequest(req); err != nil {
		return nil, err
	}
	networkName := req.Network
	if networkName == "" {
		networkName = a.cfg.Default
	}

	unlock, err := a.store.lockLeases()
	if err != nil {
		return nil, err
	}
	defer unlock()

	leases, err := a.store.readLeases()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	tap := TapName(a.cfg.TapPrefix, req.VMID, req.Index)
	mac, err := a.allocateMAC(leases, req.VMID)
	if err != nil {
		return nil, err
	}
	ip, prefix, err := a.allocateIP(leases, req.VMID)
	if err != nil {
		return nil, err
	}
	if req.Existing != nil {
		tap, mac, ip, prefix, err = a.recoverExisting(leases, req)
		if err != nil {
			return nil, err
		}
	}

	leases.CIDR = a.cfg.CIDR
	leases.Leases[ip] = &Lease{
		VMID:      req.VMID,
		MAC:       mac,
		TAP:       tap,
		CreatedAt: now,
	}
	if err := a.store.writeLeases(leases); err != nil {
		return nil, err
	}

	ips := []string{fmt.Sprintf("%s/%d", ip, prefix)}
	record := Record{
		ID:        NetworkID(req.VMID, req.Index),
		VMID:      req.VMID,
		Network:   networkName,
		Provider:  ProviderHostTap,
		IfName:    fmt.Sprintf("eth%d", req.Index),
		TAP:       tap,
		MAC:       mac,
		NumQueues: netNumQueues(req.CPU),
		QueueSize: defaultQueueSize,
		BridgeDev: a.cfg.Bridge,
		IPs:       ips,
		Gateway:   a.cfg.Gateway,
		DNS:       append([]string(nil), a.cfg.DNS...),
		Cleanup:   Cleanup{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	cfg := Config{
		ID:          record.ID,
		NetworkName: record.Network,
		TAP:         record.TAP,
		MAC:         record.MAC,
		NumQueues:   record.NumQueues,
		QueueSize:   record.QueueSize,
		Backend:     record.Provider,
		BridgeDev:   record.BridgeDev,
		IfName:      record.IfName,
		Network: &GuestInfo{
			IP:      ip,
			Gateway: record.Gateway,
			Prefix:  prefix,
			DNS:     append([]string(nil), record.DNS...),
		},
	}
	return &Allocation{Record: record, Config: cfg}, nil
}

// ReleaseIP removes a guest IP lease.
//
// The operation is idempotent so delete and rollback paths can safely retry it.
func (a *Allocator) ReleaseIP(ip string) error {
	if ip == "" {
		return nil
	}
	unlock, err := a.store.lockLeases()
	if err != nil {
		return err
	}
	defer unlock()
	leases, err := a.store.readLeases()
	if err != nil {
		return err
	}
	delete(leases.Leases, ip)
	return a.store.writeLeases(leases)
}

func (a *Allocator) allocateIP(leases *leaseIndex, vmID string) (string, int, error) {
	networkIP, ipNet, err := net.ParseCIDR(a.cfg.CIDR)
	if err != nil {
		return "", 0, fmt.Errorf("parse network CIDR: %w", err)
	}
	base := networkIP.To4()
	if base == nil {
		return "", 0, fmt.Errorf("network CIDR must be IPv4")
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return "", 0, fmt.Errorf("network CIDR must be IPv4")
	}
	gateway := net.ParseIP(a.cfg.Gateway).To4()
	for ip := nextIPv4(base); ipNet.Contains(ip); ip = nextIPv4(ip) {
		if isLastIPv4(ip, ipNet) || ip.Equal(gateway) {
			continue
		}
		ipString := ip.String()
		lease, used := leases.Leases[ipString]
		if !used || lease == nil || lease.VMID == vmID {
			return ipString, ones, nil
		}
	}
	return "", 0, fmt.Errorf("no free IP in %s", a.cfg.CIDR)
}

func (a *Allocator) allocateMAC(leases *leaseIndex, vmID string) (string, error) {
	for attempts := 0; attempts < 32; attempts++ {
		mac, err := GenerateMAC()
		if err != nil {
			return "", err
		}
		if !macInUseByOtherVM(leases, mac, vmID) {
			return mac, nil
		}
	}
	return "", fmt.Errorf("unable to generate unused MAC")
}

func (a *Allocator) recoverExisting(leases *leaseIndex, req AllocateRequest) (string, string, string, int, error) {
	existing := req.Existing
	tap := existing.TAP
	if tap == "" {
		tap = TapName(a.cfg.TapPrefix, req.VMID, req.Index)
	}
	if len(tap) > maxInterfaceNameLength {
		return "", "", "", 0, fmt.Errorf("tap name %q exceeds Linux IFNAMSIZ limit", tap)
	}
	if existing.MAC == "" {
		return "", "", "", 0, fmt.Errorf("existing network config is missing MAC")
	}
	if _, err := net.ParseMAC(existing.MAC); err != nil {
		return "", "", "", 0, fmt.Errorf("parse existing MAC: %w", err)
	}
	if macInUseByOtherVM(leases, existing.MAC, req.VMID) {
		return "", "", "", 0, fmt.Errorf("%w: MAC %s is already leased", ErrLeaseConflict, existing.MAC)
	}
	if existing.Network == nil || existing.Network.IP == "" {
		return "", "", "", 0, fmt.Errorf("existing network config is missing IP")
	}
	ip := existing.Network.IP
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil || parsedIP.To4() == nil {
		return "", "", "", 0, fmt.Errorf("existing network IP must be IPv4")
	}
	_, ipNet, err := net.ParseCIDR(a.cfg.CIDR)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("parse network CIDR: %w", err)
	}
	if !ipNet.Contains(parsedIP) {
		return "", "", "", 0, fmt.Errorf("existing network IP %s is outside %s", ip, a.cfg.CIDR)
	}
	if lease, ok := leases.Leases[ip]; ok && lease != nil && lease.VMID != req.VMID {
		return "", "", "", 0, fmt.Errorf("%w: IP %s is owned by VM %s", ErrLeaseConflict, ip, lease.VMID)
	}
	prefix := existing.Network.Prefix
	if prefix == 0 {
		prefix, _ = ipNet.Mask.Size()
	}
	return tap, strings.ToLower(existing.MAC), ip, prefix, nil
}

func macInUseByOtherVM(leases *leaseIndex, mac, vmID string) bool {
	for _, lease := range leases.Leases {
		if lease != nil && strings.EqualFold(lease.MAC, mac) && lease.VMID != vmID {
			return true
		}
	}
	return false
}

// TapName returns KumaBox's stable Linux TAP name for a VM interface.
//
// Linux interface names are limited to 15 bytes, so the VM identity is hashed
// into a short suffix instead of embedding the full VM ID.
func TapName(prefix, vmID string, index int) string {
	if prefix == "" {
		prefix = "kbtap"
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", vmID, index)))
	suffix := hex.EncodeToString(hash[:])[:8]
	name := fmt.Sprintf("%s%s", prefix, suffix)
	if len(name) > maxInterfaceNameLength {
		name = name[:maxInterfaceNameLength]
	}
	return name
}

// NetworkID returns the stable provider record ID for a VM interface.
func NetworkID(vmID string, index int) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", vmID, index)))
	return "net_" + hex.EncodeToString(hash[:])[:16]
}

// GenerateMAC returns a random locally administered unicast MAC address.
func GenerateMAC() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate MAC: %w", err)
	}
	buf[0] = (buf[0] | 0x02) & 0xfe
	return net.HardwareAddr(buf).String(), nil
}

func validateAllocateRequest(req AllocateRequest) error {
	if req.VMID == "" {
		return fmt.Errorf("vm id must not be empty")
	}
	if req.Index < 0 {
		return fmt.Errorf("network index must be non-negative")
	}
	return nil
}

func netNumQueues(cpu int) int {
	// Cloud Hypervisor validates virtio-net with a minimum of two queues. For a
	// single vCPU this still maps to one TAP queue pair on the host side.
	if cpu <= 1 {
		return 2
	}
	return cpu * 2
}

func nextIPv4(ip net.IP) net.IP {
	next := append(net.IP(nil), ip.To4()...)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

func isLastIPv4(ip net.IP, ipNet *net.IPNet) bool {
	next := nextIPv4(ip)
	return !ipNet.Contains(next)
}
