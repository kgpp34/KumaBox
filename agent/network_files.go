package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// writeNetworkFiles publishes complete MAC-matched files before removing stale
// ones. The directory is owned by the guest and contains no host-side paths.
func writeNetworkFiles(directory string, configuration NetworkConfig) error {
	if err := configuration.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	wanted := make(map[string]bool, len(configuration.Interfaces))
	for _, device := range configuration.Interfaces {
		if device.Address == "" && !device.DHCP {
			continue
		}
		name := "10-kumabox-" + strings.ReplaceAll(device.MAC, ":", "") + ".network"
		wanted[name] = true
		content := networkFileContent(device, configuration.DNSServers)
		stage, err := os.CreateTemp(directory, ".kumabox-network-")
		if err != nil {
			return err
		}
		stagePath := stage.Name()
		if _, err := stage.WriteString(content); err != nil {
			return errors.Join(err, stage.Close(), os.Remove(stagePath))
		}
		if err := errors.Join(stage.Chmod(0o644), stage.Sync(), stage.Close()); err != nil {
			return errors.Join(err, os.Remove(stagePath))
		}
		if err := os.Rename(stagePath, filepath.Join(directory, name)); err != nil {
			return errors.Join(err, os.Remove(stagePath))
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "10-kumabox-") && strings.HasSuffix(entry.Name(), ".network") && !wanted[entry.Name()] {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func networkFileContent(device NetworkInterface, dns []string) string {
	var content strings.Builder
	fmt.Fprintf(&content, "[Match]\nMACAddress=%s\n\n[Network]\n", device.MAC)
	if device.DHCP {
		content.WriteString("DHCP=ipv4\n\n[DHCPv4]\nClientIdentifier=mac\n")
	} else {
		fixedLinkLocal := net.ParseIP(device.Address).IsLinkLocalUnicast()
		if fixedLinkLocal {
			content.WriteString("DHCP=no\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n")
		}
		if !fixedLinkLocal {
			fmt.Fprintf(&content, "Address=%s/%d\n", device.Address, device.Prefix)
		}
		if device.Gateway != "" && !fixedLinkLocal {
			fmt.Fprintf(&content, "Gateway=%s\n", device.Gateway)
		}
		for _, server := range dns {
			fmt.Fprintf(&content, "DNS=%s\n", server)
		}
		if fixedLinkLocal {
			// These addresses are template-private but routable through the host
			// translator. networkd's automatic link scope prevents source address
			// selection for Internet traffic and makes resolved ignore the link.
			if device.Gateway != "" {
				content.WriteString("DNSDefaultRoute=yes\n")
			}
			fmt.Fprintf(&content, "\n[Address]\nAddress=%s/%d\nScope=global\nDuplicateAddressDetection=none\n", device.Address, device.Prefix)
			if device.Gateway != "" {
				fmt.Fprintf(&content, "\n[Route]\nDestination=0.0.0.0/0\nGateway=%s\nPreferredSource=%s\n", device.Gateway, device.Address)
			}
		}
	}
	return content.String()
}

// networkFilesMatch allows a fixed-identity clone to update its hostname
// without publishing identical network files and triggering networkd reloads.
// Both contents and stale files are checked; DHCP never uses this fast path.
func networkFilesMatch(directory string, configuration NetworkConfig) bool {
	wanted := make(map[string]string, len(configuration.Interfaces))
	for _, device := range configuration.Interfaces {
		if device.DHCP {
			return false
		}
		if device.Address == "" {
			continue
		}
		name := "10-kumabox-" + strings.ReplaceAll(device.MAC, ":", "") + ".network"
		wanted[name] = networkFileContent(device, configuration.DNSServers)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "10-kumabox-") || !strings.HasSuffix(entry.Name(), ".network") {
			continue
		}
		content, ok := wanted[entry.Name()]
		if !ok || entry.Type()&os.ModeSymlink != 0 {
			return false
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // guest-owned network directory
		if err != nil || string(data) != content {
			return false
		}
		delete(wanted, entry.Name())
	}
	return len(wanted) == 0
}

// writeFixedResolverAt uses direct DNS servers for template-local link-local
// networks. resolved deliberately excludes these links from unicast DNS even
// when they have a default route; routing through the host translator is valid.
func writeFixedResolverAt(root string, configuration NetworkConfig) error {
	applicable := false
	for _, device := range configuration.Interfaces {
		applicable = applicable || (net.ParseIP(device.Address).IsLinkLocalUnicast() && device.Gateway != "")
	}
	if !applicable || len(configuration.DNSServers) == 0 {
		return nil
	}
	var content strings.Builder
	for _, server := range configuration.DNSServers {
		fmt.Fprintf(&content, "nameserver %s\n", server)
	}
	directory := filepath.Join(root, "etc/kumabox")
	if err := os.MkdirAll(directory, 0o755); err != nil { //nolint:gosec // resolver settings must be readable by unprivileged guest processes
		return err
	}
	path := filepath.Join(directory, "resolv.conf")
	data, err := os.ReadFile(path) //nolint:gosec // fixed guest path under the supplied OS root
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if string(data) != content.String() {
		stage, err := os.CreateTemp(directory, ".resolv-")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(stage.Name()) }()
		if _, err := stage.WriteString(content.String()); err != nil {
			return errors.Join(err, stage.Close())
		}
		if err := errors.Join(stage.Chmod(0o644), stage.Sync(), stage.Close()); err != nil {
			return err
		}
		if err := os.Rename(stage.Name(), path); err != nil {
			return err
		}
	}
	resolver := filepath.Join(root, "etc/resolv.conf")
	target, err := os.Readlink(resolver)
	if err == nil && target == "/etc/kumabox/resolv.conf" {
		return nil
	}
	stage, err := os.CreateTemp(filepath.Join(root, "etc"), ".kumabox-resolver-")
	if err != nil {
		return err
	}
	name := stage.Name()
	if err := errors.Join(stage.Close(), os.Remove(name)); err != nil {
		return err
	}
	defer func() { _ = os.Remove(name) }()
	if err := os.Symlink("/etc/kumabox/resolv.conf", name); err != nil {
		return err
	}
	return os.Rename(name, resolver)
}
