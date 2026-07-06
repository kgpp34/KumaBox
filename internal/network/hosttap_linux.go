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
	"time"

	"github.com/kumabox/kumabox/internal/config"
)

var ErrNetworkConflict = errors.New("NETWORK_CONFLICT")

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

func EnsureHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig) (*HostTapReport, error) {
	return ensureHostTap(ctx, rootDir, cfg, execRunner{})
}

func TeardownHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig) (*HostTapReport, error) {
	return teardownHostTap(ctx, rootDir, cfg, execRunner{})
}

func ensureHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig, runner commandRunner) (*HostTapReport, error) {
	if err := validateHostTapConfig(cfg); err != nil {
		return nil, err
	}
	rootDir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}

	store := NewStore(rootDir)
	unlock, err := store.lockHostTap()
	if err != nil {
		return nil, err
	}
	defer unlock()

	state, err := store.readHostTapState()
	if err != nil {
		return nil, err
	}
	if err := validateHostTapOwnership(ctx, runner, rootDir, cfg, state); err != nil {
		return nil, err
	}

	report := &HostTapReport{
		Bridge:     cfg.Bridge,
		CIDR:       cfg.CIDR,
		Gateway:    cfg.Gateway,
		NATBackend: cfg.NATBackend,
	}
	created, err := ensureBridge(ctx, runner, cfg)
	if err != nil {
		return nil, err
	}
	if created {
		report.Created = true
		report.Changed = append(report.Changed, "bridge")
	}
	if changed, err := ensureGateway(ctx, runner, cfg); err != nil {
		return nil, err
	} else if changed {
		report.Changed = append(report.Changed, "gateway")
	}
	if err := setBridgeUp(ctx, runner, cfg.Bridge); err != nil {
		return nil, err
	}
	if err := ensureIPForward(ctx, runner); err != nil {
		return nil, err
	}
	if backend, changed, err := ensureNAT(ctx, runner, cfg); err != nil {
		return nil, err
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
	if err := store.writeHostTapState(state); err != nil {
		return nil, err
	}
	report.State = state
	return report, nil
}

func teardownHostTap(ctx context.Context, rootDir string, cfg config.NetworkConfig, runner commandRunner) (*HostTapReport, error) {
	rootDir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}
	store := NewStore(rootDir)
	unlock, err := store.lockHostTap()
	if err != nil {
		return nil, err
	}
	defer unlock()

	state, err := store.readHostTapState()
	if err != nil {
		return nil, err
	}
	if state == nil {
		return &HostTapReport{Bridge: cfg.Bridge, CIDR: cfg.CIDR, Gateway: cfg.Gateway, NATBackend: cfg.NATBackend}, nil
	}
	if state.Owner.Kind != "kumabox" || state.Owner.RootDir != rootDir {
		return nil, fmt.Errorf("%w: host-tap state is owned by %s at %s", ErrNetworkConflict, state.Owner.Kind, state.Owner.RootDir)
	}
	if state.RefCount > 0 {
		return nil, fmt.Errorf("%w: host-tap network still has %d reference(s)", ErrNetworkConflict, state.RefCount)
	}

	report := &HostTapReport{
		Bridge:     state.Bridge,
		CIDR:       state.CIDR,
		Gateway:    state.Gateway,
		NATBackend: state.NATBackend,
		State:      state,
	}
	if err := removeNAT(ctx, runner, state.NATBackend, state.CIDR); err != nil {
		return nil, err
	}
	report.Changed = append(report.Changed, "nat")
	if exists, err := bridgeExists(ctx, runner, state.Bridge); err != nil {
		return nil, err
	} else if exists {
		if _, err := runner.Run(ctx, "ip", "link", "delete", state.Bridge); err != nil {
			return nil, err
		}
		report.Changed = append(report.Changed, "bridge")
	}
	if err := store.removeHostTapState(); err != nil {
		return nil, err
	}
	return report, nil
}

func validateHostTapOwnership(ctx context.Context, runner commandRunner, rootDir string, cfg config.NetworkConfig, state *HostTapState) error {
	exists, err := bridgeExists(ctx, runner, cfg.Bridge)
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

func ensureBridge(ctx context.Context, runner commandRunner, cfg config.NetworkConfig) (bool, error) {
	exists, err := bridgeExists(ctx, runner, cfg.Bridge)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, err := runner.Run(ctx, "ip", "link", "add", cfg.Bridge, "type", "bridge"); err != nil {
		return false, err
	}
	return true, nil
}

func bridgeExists(ctx context.Context, runner commandRunner, bridge string) (bool, error) {
	_, err := runner.Run(ctx, "ip", "link", "show", "dev", bridge)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "Cannot find device") {
		return false, nil
	}
	return false, err
}

func ensureGateway(ctx context.Context, runner commandRunner, cfg config.NetworkConfig) (bool, error) {
	prefix, err := cidrPrefix(cfg.CIDR)
	if err != nil {
		return false, err
	}
	want := fmt.Sprintf("%s/%d", cfg.Gateway, prefix)
	out, err := runner.Run(ctx, "ip", "-4", "addr", "show", "dev", cfg.Bridge)
	if err != nil {
		return false, err
	}
	if bytes.Contains(out, []byte(want)) {
		return false, nil
	}
	if _, err := runner.Run(ctx, "ip", "addr", "add", want, "dev", cfg.Bridge); err != nil {
		return false, err
	}
	return true, nil
}

func setBridgeUp(ctx context.Context, runner commandRunner, bridge string) error {
	_, err := runner.Run(ctx, "ip", "link", "set", bridge, "up")
	return err
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
	case "none":
		return backend, false, nil
	case "iptables":
		return ensureIptablesNAT(ctx, runner, cfg.CIDR)
	case "nft":
		return ensureNftNAT(ctx, runner, cfg.CIDR)
	default:
		return "", false, fmt.Errorf("unsupported NAT backend %q", backend)
	}
}

func resolveNATBackend(configured string) (string, error) {
	switch configured {
	case "", "auto":
		if _, err := exec.LookPath("iptables"); err == nil {
			return "iptables", nil
		}
		if _, err := exec.LookPath("nft"); err == nil {
			return "nft", nil
		}
		return "", fmt.Errorf("neither iptables nor nft is available")
	case "iptables", "nft", "none":
		return configured, nil
	default:
		return "", fmt.Errorf("unsupported NAT backend %q", configured)
	}
}

func ensureIptablesNAT(ctx context.Context, runner commandRunner, cidr string) (string, bool, error) {
	_, err := runner.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	if err == nil {
		return "iptables", false, nil
	}
	if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
		return "", false, err
	}
	return "iptables", true, nil
}

func ensureNftNAT(ctx context.Context, runner commandRunner, cidr string) (string, bool, error) {
	out, _ := runner.Run(ctx, "nft", "list", "ruleset")
	if bytes.Contains(out, []byte("ip saddr "+cidr+" masquerade")) {
		return "nft", false, nil
	}
	_, _ = runner.Run(ctx, "nft", "add", "table", "inet", "kumabox")
	_, _ = runner.Run(ctx, "nft", "add", "chain", "inet", "kumabox", "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "srcnat", ";", "}")
	if _, err := runner.Run(ctx, "nft", "add", "rule", "inet", "kumabox", "postrouting", "ip", "saddr", cidr, "masquerade"); err != nil {
		return "", false, err
	}
	return "nft", true, nil
}

func removeNAT(ctx context.Context, runner commandRunner, backend, cidr string) error {
	switch backend {
	case "", "none":
		return nil
	case "iptables":
		for {
			if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
				return nil
			}
			if _, err := runner.Run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE"); err != nil {
				return err
			}
		}
	case "nft":
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
