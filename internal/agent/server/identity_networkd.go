package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const networkdIdentityPrefix = "05-kumabox-identity-"

var (
	networkdConfigDir = "/etc/systemd/network"
	networkdStateDir  = "/run/systemd/netif"
	runNetworkctl     = func(args ...string) error {
		output, err := exec.Command("networkctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("networkctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return nil
	}
)

func persistNetworkdIdentity(identities []interfaceIdentity) ([]string, error) {
	if len(identities) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(networkdConfigDir, 0o755); err != nil {
		return nil, fmt.Errorf("create networkd config directory: %w", err)
	}

	desired := make(map[string]struct{}, len(identities))
	names := make([]string, 0, len(identities))
	for index, identity := range identities {
		content, filename, err := renderNetworkdIdentity(identity)
		if err != nil {
			return nil, fmt.Errorf("interface %d: %w", index, err)
		}
		path := filepath.Join(networkdConfigDir, filename)
		if err := writeAtomic(path, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		desired[filename] = struct{}{}
		names = append(names, identity.Name)
	}
	if err := removeStaleNetworkdIdentities(desired); err != nil {
		return nil, err
	}
	return names, nil
}

func renderNetworkdIdentity(identity interfaceIdentity) (string, string, error) {
	mac, err := net.ParseMAC(identity.MAC)
	if err != nil {
		return "", "", fmt.Errorf("invalid MAC %s", identity.MAC)
	}
	if identity.Name == "" || strings.ContainsAny(identity.Name, "/\x00\n") {
		return "", "", fmt.Errorf("invalid interface name %q", identity.Name)
	}
	if identity.IP == "" || identity.Prefix < 1 || identity.Prefix > 32 || net.ParseIP(identity.IP).To4() == nil {
		return "", "", fmt.Errorf("invalid IPv4 address %s/%d", identity.IP, identity.Prefix)
	}
	if identity.Gateway != "" && net.ParseIP(identity.Gateway).To4() == nil {
		return "", "", fmt.Errorf("invalid gateway %s", identity.Gateway)
	}
	for _, dns := range identity.DNS {
		if net.ParseIP(dns) == nil {
			return "", "", fmt.Errorf("invalid DNS server %s", dns)
		}
	}

	canonicalMAC := strings.ToLower(mac.String())
	filename := networkdIdentityPrefix + strings.ReplaceAll(canonicalMAC, ":", "") + ".network"
	var content strings.Builder
	fmt.Fprintf(&content, "[Match]\nMACAddress=%s\n\n", canonicalMAC)
	content.WriteString("[Network]\nDHCP=no\nLinkLocalAddressing=ipv6\n")
	fmt.Fprintf(&content, "Address=%s/%d\n", identity.IP, identity.Prefix)
	if identity.Gateway != "" {
		fmt.Fprintf(&content, "Gateway=%s\n", identity.Gateway)
	}
	for _, dns := range identity.DNS {
		fmt.Fprintf(&content, "DNS=%s\n", dns)
	}
	return content.String(), filename, nil
}

func removeStaleNetworkdIdentities(desired map[string]struct{}) error {
	entries, err := os.ReadDir(networkdConfigDir)
	if err != nil {
		return fmt.Errorf("list networkd identity configs: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), networkdIdentityPrefix) {
			continue
		}
		if _, ok := desired[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(networkdConfigDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale networkd identity %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func reloadNetworkd() error {
	if _, err := os.Stat(networkdStateDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect networkd state: %w", err)
	}
	return runNetworkctl("reload")
}

func reconfigureNetworkd(interfaceNames []string) error {
	if _, err := os.Stat(networkdStateDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect networkd state: %w", err)
	}
	sort.Strings(interfaceNames)
	for _, name := range interfaceNames {
		if err := runNetworkctl("reconfigure", name); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, content []byte, mode os.FileMode) (retErr error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kumabox-network-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			if err := tmp.Close(); err != nil && retErr == nil {
				retErr = err
			}
		}
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}
