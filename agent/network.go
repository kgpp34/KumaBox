package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
)

var (
	// ErrNetworkConfigUnsupported means the guest agent predates this protocol
	// operation. Hosts may use the legacy exec path for that specific response.
	ErrNetworkConfigUnsupported = errors.New("guest agent does not support network configuration")
	validGuestHostname          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
)

// NetworkInterface contains only guest-visible facts, never host TAP paths.
type NetworkInterface struct {
	MAC     string `json:"mac"`
	Address string `json:"address,omitempty"`
	Prefix  int    `json:"prefix,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// NetworkConfig replaces all managed NIC settings and the guest hostname.
type NetworkConfig struct {
	Hostname   string             `json:"hostname"`
	Interfaces []NetworkInterface `json:"interfaces"`
	DNSServers []string           `json:"dns_servers,omitempty"`
}

// Validate checks the complete request before the guest changes files.
func (c NetworkConfig) Validate() error {
	if !validGuestHostname.MatchString(c.Hostname) || len(c.Interfaces) > 64 || len(c.DNSServers) > 2 {
		return errors.New("invalid guest hostname, NIC count, or DNS count")
	}
	for _, server := range c.DNSServers {
		if net.ParseIP(server) == nil {
			return fmt.Errorf("invalid guest DNS address %q", server)
		}
	}
	seen := make(map[string]bool, len(c.Interfaces))
	for _, device := range c.Interfaces {
		mac, err := net.ParseMAC(device.MAC)
		if err != nil || len(mac) != 6 || seen[device.MAC] {
			return fmt.Errorf("invalid or duplicate guest MAC %q", device.MAC)
		}
		seen[device.MAC] = true
		if device.Address == "" {
			if device.Prefix != 0 || device.Gateway != "" {
				return fmt.Errorf("guest NIC %s has settings without an address", device.MAC)
			}
			continue
		}
		if net.ParseIP(device.Address).To4() == nil || device.Prefix < 0 || device.Prefix > 32 {
			return fmt.Errorf("invalid guest IPv4 address or prefix for %s", device.MAC)
		}
		if device.Gateway != "" && net.ParseIP(device.Gateway).To4() == nil {
			return fmt.Errorf("invalid guest gateway for %s", device.MAC)
		}
	}
	return nil
}

// ConfigureNetwork sends one structured request over an already connected
// transport. The caller closes the connection after the response.
func ConfigureNetwork(ctx context.Context, connection io.ReadWriteCloser, configuration NetworkConfig) error {
	if err := configuration.Validate(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := NewEncoder(connection).Encode(Message{Type: MessageConfigureNetwork, Network: &configuration}); err != nil {
		return fmt.Errorf("send network configuration: %w", err)
	}
	response, err := NewDecoder(connection).Decode()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("read network response: %w", err)
	}
	switch response.Type {
	case MessageExit:
		if response.ExitCode != 0 {
			return fmt.Errorf("guest network configuration exited with status %d", response.ExitCode)
		}
		return nil
	case MessageError:
		if strings.HasPrefix(response.Message, "expected first frame type ") {
			return ErrNetworkConfigUnsupported
		}
		return fmt.Errorf("guest agent: %s", response.Message)
	default:
		return fmt.Errorf("unexpected network response %q", response.Type)
	}
}
