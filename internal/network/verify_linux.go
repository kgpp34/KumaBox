//go:build linux

package network

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

func verifyConfig(config Config) error {
	switch config.Backend {
	case ProviderCNI:
		return verifyCNIConfig(config)
	case ProviderHostTap:
		return verifyHostTapConfig(config)
	case ProviderNone, "":
		return nil
	default:
		return fmt.Errorf("unsupported network backend %q", config.Backend)
	}
}

func verifyHostTapConfig(config Config) error {
	tap, err := netlink.LinkByName(config.TAP)
	if err != nil {
		if isLinkNotFound(err) {
			return fmt.Errorf("%w: tap %s is missing", ErrNetworkUnavailable, config.TAP)
		}
		return fmt.Errorf("find tap %s: %w", config.TAP, err)
	}
	if tap.Type() != "tun" {
		return fmt.Errorf("%w: link %s has type %s, want tun", ErrNetworkConflict, config.TAP, tap.Type())
	}
	bridge, err := netlink.LinkByName(config.BridgeDev)
	if err != nil {
		if isLinkNotFound(err) {
			return fmt.Errorf("%w: bridge %s is missing", ErrNetworkUnavailable, config.BridgeDev)
		}
		return fmt.Errorf("find bridge %s: %w", config.BridgeDev, err)
	}
	if tap.Attrs().MasterIndex != bridge.Attrs().Index {
		return fmt.Errorf("%w: tap %s is not attached to bridge %s", ErrNetworkConflict, config.TAP, config.BridgeDev)
	}
	if tap.Attrs().Flags&net.FlagUp == 0 || bridge.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("%w: tap %s or bridge %s is down", ErrNetworkUnavailable, config.TAP, config.BridgeDev)
	}
	return nil
}

func verifyCNIConfig(config Config) error {
	if config.NetnsPath == "" {
		return fmt.Errorf("%w: CNI network namespace path is empty", ErrNetworkUnavailable)
	}
	if _, err := os.Stat(config.NetnsPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: CNI network namespace %s is missing", ErrNetworkUnavailable, config.NetnsPath)
		}
		return fmt.Errorf("stat CNI network namespace %s: %w", config.NetnsPath, err)
	}
	return withNetNSPath(config.NetnsPath, func() error {
		guest, err := netlink.LinkByName(config.IfName)
		if err != nil {
			if isLinkNotFound(err) {
				return fmt.Errorf("%w: CNI link %s is missing", ErrNetworkUnavailable, config.IfName)
			}
			return fmt.Errorf("find CNI link %s: %w", config.IfName, err)
		}
		tap, err := netlink.LinkByName(config.TAP)
		if err != nil {
			if isLinkNotFound(err) {
				return fmt.Errorf("%w: CNI tap %s is missing", ErrNetworkUnavailable, config.TAP)
			}
			return fmt.Errorf("find CNI tap %s: %w", config.TAP, err)
		}
		if guest.Attrs().Flags&net.FlagUp == 0 || tap.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("%w: CNI link %s or tap %s is down", ErrNetworkUnavailable, config.IfName, config.TAP)
		}
		if config.MAC != "" && guest.Attrs().HardwareAddr != nil &&
			!equalMAC(config.MAC, guest.Attrs().HardwareAddr) {
			return fmt.Errorf("%w: CNI link %s MAC is %s, want %s", ErrNetworkConflict,
				config.IfName, guest.Attrs().HardwareAddr, config.MAC)
		}
		return nil
	})
}

func equalMAC(want string, got net.HardwareAddr) bool {
	parsed, err := net.ParseMAC(want)
	return err == nil && parsed.String() == got.String()
}
