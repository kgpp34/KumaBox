package agent

import (
	"fmt"
	"net"
	"strings"
)

// LegacyNetworkScript preserves compatibility with guest images that support
// exec but predate the structured network request. It is removed only after
// those images are no longer supported.
func LegacyNetworkScript(configuration NetworkConfig) (string, error) {
	if err := configuration.Validate(); err != nil {
		return "", err
	}
	var script strings.Builder
	script.WriteString("set -eu\nmkdir -p /etc/systemd/network\nrm -f /etc/systemd/network/10-kumabox-*.network\n")
	for _, device := range configuration.Interfaces {
		if device.Address == "" {
			continue
		}
		filename := strings.ReplaceAll(device.MAC, ":", "")
		fmt.Fprintf(&script, "cat > /etc/systemd/network/10-kumabox-%s.network <<'KUMABOX_NETWORK'\n", filename)
		fmt.Fprintf(&script, "[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n", device.MAC, device.Address, device.Prefix)
		if device.Gateway != "" {
			fmt.Fprintf(&script, "Gateway=%s\n", device.Gateway)
		}
		for _, server := range configuration.DNSServers {
			fmt.Fprintf(&script, "DNS=%s\n", server)
		}
		script.WriteString("KUMABOX_NETWORK\n")
	}
	fmt.Fprintf(&script, "printf '%%s\n' '%s' > /etc/hostname\nhostname '%s'\n", configuration.Hostname, configuration.Hostname)
	if len(configuration.Interfaces) > 0 {
		script.WriteString("systemctl restart systemd-networkd\n")
	}
	return script.String(), nil
}

// LegacyQuiesceNICScript lowers a MAC-matched guest link on agents that only
// support exec. The MAC is validated before it reaches the shell command.
func LegacyQuiesceNICScript(mac string) (string, error) {
	parsed, err := net.ParseMAC(mac)
	if err != nil || len(parsed) != 6 {
		return "", fmt.Errorf("invalid guest MAC %q", mac)
	}
	return fmt.Sprintf("for net in /sys/class/net/*; do [ \"$(cat \"$net/address\")\" = '%s' ] || continue; ip link set \"${net##*/}\" down; done", mac), nil
}
