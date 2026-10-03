package agent

import (
	"errors"
	"fmt"
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
		if device.Address == "" {
			continue
		}
		name := "10-kumabox-" + strings.ReplaceAll(device.MAC, ":", "") + ".network"
		wanted[name] = true
		var content strings.Builder
		fmt.Fprintf(&content, "[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n", device.MAC, device.Address, device.Prefix)
		if device.Gateway != "" {
			fmt.Fprintf(&content, "Gateway=%s\n", device.Gateway)
		}
		for _, server := range configuration.DNSServers {
			fmt.Fprintf(&content, "DNS=%s\n", server)
		}
		stage, err := os.CreateTemp(directory, ".kumabox-network-")
		if err != nil {
			return err
		}
		stagePath := stage.Name()
		if _, err := stage.WriteString(content.String()); err != nil {
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
