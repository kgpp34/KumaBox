//go:build linux

package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/vishvananda/netlink"
)

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// EnsureHostTap creates or reconciles the global host-tap bridge.
//
// The bridge, gateway address, IP forwarding, and NAT rule are shared by VMs
// under one KumaBox root. Ownership is recorded on disk so another root cannot
// accidentally tear down or reconfigure the same host device.
func EnsureHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig) (*HostTapReport, error) {
	return EnsureHostTapWithStore(ctx, rootDir, cfg, NewStore(rootDir))
}

// EnsureHostTapWithStore reconciles host-tap state in the caller's metadata
// backend instead of opening an independent JSON store.
func EnsureHostTapWithStore(
	ctx context.Context,
	rootDir string,
	cfg config.NetworkConfig,
	store *Store,
) (*HostTapReport, error) {
	return ensureHostTap(ctx, rootDir, cfg, store, execRunner{})
}

// TeardownHostTap removes the global host-tap bridge and NAT rule.
//
// Teardown refuses to run while HostTapState.RefCount is non-zero. VM delete is
// responsible for deleting per-VM taps and decrementing that reference count.
func TeardownHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig) (*HostTapReport, error) {
	return teardownHostTap(ctx, rootDir, cfg, execRunner{})
}

func ensureHostTap(
	ctx context.Context,
	rootDir string,
	cfg config.NetworkConfig,
	store *Store,
	runner commandRunner,
) (*HostTapReport, error) {
	if err := validateHostTapConfig(cfg); err != nil {
		return nil, err
	}
	rootDir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}

	var report *HostTapReport
	err = store.withHostTap(true, func(current **HostTapState) error {
		state := *current
		if err := validateHostTapOwnership(rootDir, cfg, state); err != nil {
			return err
		}
		report = &HostTapReport{Bridge: cfg.Bridge, CIDR: cfg.CIDR, Gateway: cfg.Gateway, NATBackend: cfg.NATBackend}
		created, err := ensureBridge(cfg)
		if err != nil {
			return err
		}
		if created {
			report.Created = true
			report.Changed = append(report.Changed, "bridge")
		}
		if changed, err := ensureGateway(cfg); err != nil {
			return err
		} else if changed {
			report.Changed = append(report.Changed, "gateway")
		}
		if err := setBridgeUp(cfg.Bridge); err != nil {
			return err
		}
		if err := ensureIPForward(ctx, runner); err != nil {
			return err
		}
		if backend, changed, err := ensureNAT(ctx, runner, cfg); err != nil {
			return err
		} else {
			report.NATBackend = backend
			if changed {
				report.Changed = append(report.Changed, "nat")
			}
		}

		now := time.Now().UTC()
		if state == nil {
			state = &HostTapState{CreatedAt: now}
		}
		state.SchemaVersion = hostTapSchemaVersion
		state.Bridge = cfg.Bridge
		state.CIDR = cfg.CIDR
		state.Gateway = cfg.Gateway
		state.NATBackend = report.NATBackend
		state.Owner = Owner{Kind: "kumabox", RootDir: rootDir}
		state.UpdatedAt = now
		if state.CreatedAt.IsZero() {
			state.CreatedAt = now
		}
		*current = state
		report.State = state
		return nil
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

func teardownHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig, runner commandRunner) (*HostTapReport, error) {
	rootDir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}
	store := NewStore(rootDir)
	var report *HostTapReport
	err = store.withHostTap(true, func(current **HostTapState) error {
		state := *current
		if state == nil {
			report = &HostTapReport{Bridge: cfg.Bridge, CIDR: cfg.CIDR, Gateway: cfg.Gateway, NATBackend: cfg.NATBackend}
			return nil
		}
		if state.Owner.Kind != "kumabox" || state.Owner.RootDir != rootDir {
			return fmt.Errorf("%w: host-tap state is owned by %s at %s", ErrNetworkConflict, state.Owner.Kind, state.Owner.RootDir)
		}
		if state.RefCount > 0 {
			return fmt.Errorf("%w: host-tap network still has %d reference(s)", ErrNetworkConflict, state.RefCount)
		}

		report = &HostTapReport{
			Bridge:     state.Bridge,
			CIDR:       state.CIDR,
			Gateway:    state.Gateway,
			NATBackend: state.NATBackend,
			State:      state,
		}
		if err := removeNAT(ctx, runner, state.NATBackend, state.CIDR); err != nil {
			return err
		}
		report.Changed = append(report.Changed, "nat")
		if exists, err := bridgeExists(state.Bridge); err != nil {
			return err
		} else if exists {
			if err := deleteBridge(state.Bridge); err != nil {
				return err
			}
			report.Changed = append(report.Changed, "bridge")
		}
		*current = nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

func validateHostTapOwnership(rootDir string, cfg config.NetworkConfig, state *HostTapState) error {
	exists, err := bridgeExists(cfg.Bridge)
	if err != nil {
		return err
	}
	if state == nil {
		if exists {
			return fmt.Errorf("%w: bridge %s already exists without KumaBox owner state", ErrNetworkConflict, cfg.Bridge)
		}
		return nil
	}
	if state.Owner.Kind != "kumabox" || state.Owner.RootDir != rootDir {
		return fmt.Errorf("%w: bridge %s is owned by %s at %s", ErrNetworkConflict, cfg.Bridge, state.Owner.Kind, state.Owner.RootDir)
	}
	if state.Bridge != "" && state.Bridge != cfg.Bridge {
		return fmt.Errorf("%w: host-tap state bridge %s does not match configured bridge %s", ErrNetworkConflict, state.Bridge, cfg.Bridge)
	}
	return nil
}

func validateHostTapConfig(cfg config.NetworkConfig) error {
	if cfg.Bridge == "" {
		return fmt.Errorf("network bridge must not be empty")
	}
	if cfg.CIDR == "" {
		return fmt.Errorf("network CIDR must not be empty")
	}
	if cfg.Gateway == "" {
		return fmt.Errorf("network gateway must not be empty")
	}
	gateway := net.ParseIP(cfg.Gateway)
	if gateway == nil || gateway.To4() == nil {
		return fmt.Errorf("network gateway must be IPv4")
	}
	_, ipNet, err := net.ParseCIDR(cfg.CIDR)
	if err != nil {
		return fmt.Errorf("parse network CIDR: %w", err)
	}
	if !ipNet.Contains(gateway) {
		return fmt.Errorf("network gateway %s is outside %s", cfg.Gateway, cfg.CIDR)
	}
	return nil
}

func ensureBridge(cfg config.NetworkConfig) (bool, error) {
	exists, err := bridgeExists(cfg.Bridge)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: cfg.Bridge,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return false, err
	}
	return true, nil
}

func bridgeExists(bridge string) (bool, error) {
	if _, err := netlink.LinkByName(bridge); err == nil {
		return true, nil
	} else if isLinkNotFound(err) {
		return false, nil
	} else {
		return false, err
	}
}

func ensureGateway(cfg config.NetworkConfig) (bool, error) {
	link, err := netlink.LinkByName(cfg.Bridge)
	if err != nil {
		return false, err
	}
	prefix, err := cidrPrefix(cfg.CIDR)
	if err != nil {
		return false, err
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(cfg.Gateway).To4(),
			Mask: net.CIDRMask(prefix, 32),
		},
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	for _, existing := range addrs {
		if existing.IP.Equal(addr.IP) && bytes.Equal(existing.Mask, addr.Mask) {
			return false, nil
		}
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func setBridgeUp(bridge string) error {
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

func deleteBridge(bridge string) error {
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound)
}

func ensureIPForward(ctx context.Context, runner commandRunner) error {
	_, err := runner.Run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1")
	return err
}

func ensureNAT(ctx context.Context, runner commandRunner, cfg config.NetworkConfig) (string, bool, error) {
	backend, err := resolveNATBackend(cfg.NATBackend)
	if err != nil {
		return "", false, err
	}
	switch backend {
	case NATBackendNone:
		return backend, false, nil
	case NATBackendIPTables:
		return ensureIptablesNAT(ctx, runner, cfg.CIDR)
	case NATBackendNFT:
		return ensureNftNAT(ctx, runner, cfg.CIDR)
	default:
		return "", false, fmt.Errorf("unsupported NAT backend %q", backend)
	}
}

func resolveNATBackend(configured string) (string, error) {
	switch configured {
	case "", NATBackendAuto:
		if _, err := exec.LookPath("iptables"); err == nil {
			return NATBackendIPTables, nil
		}
		if _, err := exec.LookPath("nft"); err == nil {
			return NATBackendNFT, nil
		}
		return "", fmt.Errorf("neither iptables nor nft is available")
	case NATBackendIPTables, NATBackendNFT, NATBackendNone:
		return configured, nil
	default:
		return "", fmt.Errorf("unsupported NAT backend %q", configured)
	}
}

func ensureIptablesNAT(ctx context.Context, runner commandRunner, cidr string) (string, bool, error) {
	_, err := runner.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	if err == nil {
		return NATBackendIPTables, false, nil
	}
	if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
		return "", false, err
	}
	return NATBackendIPTables, true, nil
}

func ensureNftNAT(ctx context.Context, runner commandRunner, cidr string) (string, bool, error) {
	out, _ := runner.Run(ctx, "nft", "list", "ruleset")
	if bytes.Contains(out, []byte("ip saddr "+cidr+" masquerade")) {
		return NATBackendNFT, false, nil
	}
	_, _ = runner.Run(ctx, "nft", "add", "table", "inet", "kumabox")
	_, _ = runner.Run(ctx, "nft", "add", "chain", "inet", "kumabox", "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "srcnat", ";", "}")
	if _, err := runner.Run(ctx, "nft", "add", "rule", "inet", "kumabox", "postrouting", "ip", "saddr", cidr, "masquerade"); err != nil {
		return "", false, err
	}
	return NATBackendNFT, true, nil
}

func removeNAT(ctx context.Context, runner commandRunner, backend, cidr string) error {
	switch backend {
	case "", NATBackendNone:
		return nil
	case NATBackendIPTables:
		for {
			if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
				return nil
			}
			if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
				return err
			}
		}
	case NATBackendNFT:
		_, _ = runner.Run(ctx, "nft", "delete", "table", "inet", "kumabox")
		return nil
	default:
		return fmt.Errorf("unsupported NAT backend %q", backend)
	}
}

func cidrPrefix(cidr string) (int, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("parse network CIDR: %w", err)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return 0, fmt.Errorf("network CIDR must be IPv4")
	}
	return ones, nil
}
