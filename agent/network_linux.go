//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// applyGuestNetwork owns the guest OS mechanism behind the wire protocol.
func applyGuestNetwork(ctx context.Context, configuration NetworkConfig) error {
	if err := writeNetworkFiles("/etc/systemd/network", configuration); err != nil {
		return fmt.Errorf("write network files: %w", err)
	}
	if err := os.WriteFile("/etc/hostname", []byte(configuration.Hostname+"\n"), 0o644); err != nil { //nolint:gosec // fixed guest OS path
		return fmt.Errorf("write hostname: %w", err)
	}
	if err := unix.Sethostname([]byte(configuration.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if len(configuration.Interfaces) == 0 {
		return nil
	}
	output, err := exec.CommandContext(ctx, "systemctl", "restart", "systemd-networkd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart systemd-networkd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
