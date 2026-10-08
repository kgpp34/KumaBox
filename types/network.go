package types

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
)

// NetworkBackend identifies the host networking implementation that owns a
// sandbox's durable network resources.
type NetworkBackend string

const (
	// NetworkBackendCNI selects a CNI plugin chain running in a private network
	// namespace.
	NetworkBackendCNI NetworkBackend = "cni"
)

// Validate rejects backend names that cannot be routed to an implementation.
func (b NetworkBackend) Validate() error {
	switch b {
	case NetworkBackendCNI:
		return nil
	default:
		return fmt.Errorf("unsupported network backend %q", b)
	}
}

// IPv4Config is the guest-visible address returned by an infrastructure
// network provider.
type IPv4Config struct {
	// Address is one IPv4 address without its prefix length.
	Address string
	// Gateway is an optional IPv4 default gateway.
	Gateway string
	// Prefix is the CIDR prefix length in bits.
	Prefix int
}

// Validate rejects malformed or non-IPv4 addresses before they are persisted
// or rendered into the guest boot contract.
func (c IPv4Config) Validate() error {
	ip := net.ParseIP(c.Address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("network address %q is not IPv4", c.Address)
	}
	if c.Prefix < 0 || c.Prefix > 32 {
		return fmt.Errorf("network prefix %d is outside 0..32", c.Prefix)
	}
	if c.Gateway != "" {
		gateway := net.ParseIP(c.Gateway)
		if gateway == nil || gateway.To4() == nil {
			return fmt.Errorf("network gateway %q is not IPv4", c.Gateway)
		}
	}
	return nil
}

// NetworkInterface contains the durable handoff from host networking to a VMM.
// Provider-private cleanup phases and CNI record identifiers are deliberately
// excluded from this shared value object.
type NetworkInterface struct {
	// Index is the zero-based NIC position used to derive the guest name.
	Index int
	// Name is the interface name created by CNI inside the private namespace.
	Name string
	// TAP is the device opened by the VMM.
	TAP string
	// MAC is the stable guest hardware address.
	MAC string
	// Queues is the total RX and TX virtio queue count.
	Queues int
	// QueueSize is the descriptor count for each virtio queue.
	QueueSize int
	// Network is the resolved CNI conflist name.
	Network string
	// IPv4 is nil when a plugin intentionally returns no IPv4 address.
	IPv4 *IPv4Config
}

// Validate checks the provider-to-VMM handoff independently of persistence and
// command presentation.
func (c NetworkInterface) Validate() error {
	if c.Index < 0 {
		return errors.New("network interface index must not be negative")
	}
	if c.Name != "eth"+strconv.Itoa(c.Index) {
		return fmt.Errorf("network interface %d must be named eth%d", c.Index, c.Index)
	}
	if c.TAP == "" || c.Network == "" {
		return errors.New("network interface requires TAP and network names")
	}
	if _, err := net.ParseMAC(c.MAC); err != nil {
		return fmt.Errorf("network interface MAC %q: %w", c.MAC, err)
	}
	if c.Queues < 2 || c.Queues%2 != 0 || c.QueueSize <= 0 {
		return errors.New("network interface requires an even queue count of at least two and a positive queue size")
	}
	if c.IPv4 != nil {
		if err := c.IPv4.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// NetworkSetup is the complete durable network state of one sandbox. Its zero
// value represents a sandbox created without networking.
type NetworkSetup struct {
	// Backend selects the provider used by later lifecycle operations.
	Backend NetworkBackend
	// Namespace is the absolute Linux network namespace path containing the
	// CNI interfaces and TAP devices.
	Namespace string
	// Interfaces are ordered by their stable NIC index.
	Interfaces []NetworkInterface
}

// Validate accepts the disabled zero value and otherwise checks a complete,
// deterministic provider handoff.
func (s NetworkSetup) Validate() error {
	if s.Backend == "" {
		if s.Namespace != "" || len(s.Interfaces) != 0 {
			return errors.New("network setup without a backend must be empty")
		}
		return nil
	}
	if err := s.Backend.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(s.Namespace) {
		return errors.New("network namespace must be an absolute path")
	}
	seen := make(map[int]struct{}, len(s.Interfaces))
	previous := -1
	for position, networkInterface := range s.Interfaces {
		if err := networkInterface.Validate(); err != nil {
			return fmt.Errorf("network interface %d: %w", networkInterface.Index, err)
		}
		if _, exists := seen[networkInterface.Index]; exists {
			return fmt.Errorf("network interface index %d is duplicated", networkInterface.Index)
		}
		if networkInterface.Index <= previous {
			return errors.New("network interfaces must be ordered by increasing index")
		}
		if networkInterface.Index != position {
			return errors.New("network interface indices must be contiguous from zero")
		}
		seen[networkInterface.Index] = struct{}{}
		previous = networkInterface.Index
	}
	return nil
}
