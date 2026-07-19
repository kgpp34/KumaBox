// Package config owns KumaBox's process-level configuration model.
//
// Configuration is intentionally layered: compiled defaults are loaded first,
// an optional TOML file may replace them, and CLI overrides win last. Runtime
// code should receive a fully validated Config instead of reading flags or
// environment variables directly.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

const (
	defaultRootDir               = "/var/lib/kumabox"
	defaultRunDir                = "/run/kumabox"
	defaultLogDir                = "/var/log/kumabox"
	defaultCloudHypervisorBinary = "cloud-hypervisor"
	defaultQEMUImgBinary         = "qemu-img"
	defaultAPISocketTimeoutMS    = 5000
	defaultStopTimeoutMS         = 10000
	defaultNetworkMode           = "cni"
	defaultNetworkName           = "default"
	defaultBridge                = "kumabox0"
	defaultCIDR                  = "10.88.0.0/16"
	defaultGateway               = "10.88.0.1"
	defaultTapPrefix             = "kbtap"
	defaultNATBackend            = "auto"
	defaultCNIConfigDir          = "/etc/cni/net.d"
	defaultCNIBinDir             = "/opt/cni/bin"
)

var defaultDNS = []string{"1.1.1.1", "8.8.8.8"}

// Config is the complete configuration snapshot used by a KumaBox command.
//
// The value is treated as immutable after Load returns. Packages that need
// paths or provider settings receive this struct explicitly so tests can use
// isolated root/run/log directories without mutating global process state.
type Config struct {
	Runtime RuntimeConfig `toml:"runtime" json:"runtime"`
	Backend BackendConfig `toml:"backend" json:"backend"`
	Network NetworkConfig `toml:"network" json:"network"`
	Storage StorageConfig `toml:"storage" json:"storage"`
}

// StorageConfig controls host tools used to prepare durable VM disks.
type StorageConfig struct {
	QEMUImgBinary string `toml:"qemu_img_binary" json:"qemuImgBinary"`
}

// RuntimeConfig contains the three host path roots used by KumaBox.
//
// RootDir is durable state such as VM/image indexes and network leases. RunDir
// is ephemeral runtime state such as sockets and rendered VMM config. LogDir is
// command-readable VM output and event logs.
type RuntimeConfig struct {
	RootDir string `toml:"root_dir" json:"rootDir"`
	RunDir  string `toml:"run_dir" json:"runDir"`
	LogDir  string `toml:"log_dir" json:"logDir"`
}

// BackendConfig contains backend-specific runtime configuration.
type BackendConfig struct {
	CloudHypervisor CloudHypervisorConfig `toml:"cloud_hypervisor" json:"cloudHypervisor"`
}

// CloudHypervisorConfig controls the Cloud Hypervisor binary and timeouts.
type CloudHypervisorConfig struct {
	Binary             string `toml:"binary" json:"binary"`
	APISocketTimeoutMS int    `toml:"api_socket_timeout_ms" json:"apiSocketTimeoutMs"`
	StopTimeoutMS      int    `toml:"stop_timeout_ms" json:"stopTimeoutMs"`
}

// NetworkConfig contains host networking defaults used by network providers.
//
// The host-tap provider owns a single bridge/NAT domain per RootDir. CIDR and
// Gateway define the guest address pool; TapPrefix is constrained by Linux's
// interface-name limit after KumaBox appends a stable hash suffix.
type NetworkConfig struct {
	Mode         string   `toml:"mode" json:"mode"`
	Default      string   `toml:"default" json:"default"`
	Bridge       string   `toml:"bridge" json:"bridge"`
	CIDR         string   `toml:"cidr" json:"cidr"`
	Gateway      string   `toml:"gateway" json:"gateway"`
	DNS          []string `toml:"dns" json:"dns"`
	TapPrefix    string   `toml:"tap_prefix" json:"tapPrefix"`
	NATBackend   string   `toml:"nat_backend" json:"natBackend"`
	CNIConfigDir string   `toml:"cni_config_dir" json:"cniConfigDir"`
	CNIBinDir    string   `toml:"cni_bin_dir" json:"cniBinDir"`
}

// Overrides contains command-line values that replace file or default config.
type Overrides struct {
	RootDir            string
	RunDir             string
	LogDir             string
	CloudHypervisorBin string
	QEMUImgBinary      string
}

// Load reads config from path, applies overrides, and validates the result.
//
// A missing path means "use defaults plus overrides". When path is non-empty it
// must exist and contain TOML compatible with Config.
func Load(path string, overrides Overrides) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := toml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyOverrides(&cfg, overrides)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Default returns the built-in KumaBox configuration.
//
// The default network uses the host's CNI configuration. The built-in
// host-tap values remain available for the explicit compatibility provider.
func Default() Config {
	return Config{
		Runtime: RuntimeConfig{
			RootDir: defaultRootDir,
			RunDir:  defaultRunDir,
			LogDir:  defaultLogDir,
		},
		Backend: BackendConfig{
			CloudHypervisor: CloudHypervisorConfig{
				Binary:             defaultCloudHypervisorBinary,
				APISocketTimeoutMS: defaultAPISocketTimeoutMS,
				StopTimeoutMS:      defaultStopTimeoutMS,
			},
		},
		Network: NetworkConfig{
			Mode:         defaultNetworkMode,
			Default:      defaultNetworkName,
			Bridge:       defaultBridge,
			CIDR:         defaultCIDR,
			Gateway:      defaultGateway,
			DNS:          append([]string(nil), defaultDNS...),
			TapPrefix:    defaultTapPrefix,
			NATBackend:   defaultNATBackend,
			CNIConfigDir: defaultCNIConfigDir,
			CNIBinDir:    defaultCNIBinDir,
		},
		Storage: StorageConfig{QEMUImgBinary: defaultQEMUImgBinary},
	}
}

// EnsureRuntimeDirs creates the configured runtime directories.
//
// Callers should do this before rendering VM config, writing indexes, or
// creating host networking state. The function creates only the configured
// roots; per-VM subdirectories remain owned by runtime/backend code.
func EnsureRuntimeDirs(cfg Config) error {
	for _, dir := range []string{cfg.Runtime.RootDir, cfg.Runtime.RunDir, cfg.Runtime.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

func applyOverrides(cfg *Config, overrides Overrides) {
	if overrides.RootDir != "" {
		cfg.Runtime.RootDir = overrides.RootDir
	}
	if overrides.RunDir != "" {
		cfg.Runtime.RunDir = overrides.RunDir
	}
	if overrides.LogDir != "" {
		cfg.Runtime.LogDir = overrides.LogDir
	}
	if overrides.CloudHypervisorBin != "" {
		cfg.Backend.CloudHypervisor.Binary = overrides.CloudHypervisorBin
	}
	if overrides.QEMUImgBinary != "" {
		cfg.Storage.QEMUImgBinary = overrides.QEMUImgBinary
	}
}

func validate(cfg Config) error {
	if cfg.Runtime.RootDir == "" {
		return errors.New("runtime.root_dir must not be empty")
	}
	if cfg.Runtime.RunDir == "" {
		return errors.New("runtime.run_dir must not be empty")
	}
	if cfg.Runtime.LogDir == "" {
		return errors.New("runtime.log_dir must not be empty")
	}
	if cfg.Backend.CloudHypervisor.Binary == "" {
		return errors.New("backend.cloud_hypervisor.binary must not be empty")
	}
	if cfg.Storage.QEMUImgBinary == "" {
		return errors.New("storage.qemu_img_binary must not be empty")
	}
	if cfg.Network.Mode == "" {
		return errors.New("network.mode must not be empty")
	}
	if cfg.Network.Default == "" {
		return errors.New("network.default must not be empty")
	}
	if cfg.Network.Mode != "host-tap" && cfg.Network.Mode != "none" && cfg.Network.Mode != "cni" {
		return fmt.Errorf("network.mode must be one of host-tap, cni, or none")
	}
	if cfg.Network.Mode == "host-tap" {
		if cfg.Network.Bridge == "" {
			return errors.New("network.bridge must not be empty when network.mode is host-tap")
		}
		if cfg.Network.CIDR == "" {
			return errors.New("network.cidr must not be empty when network.mode is host-tap")
		}
		if cfg.Network.Gateway == "" {
			return errors.New("network.gateway must not be empty when network.mode is host-tap")
		}
		if cfg.Network.TapPrefix == "" {
			return errors.New("network.tap_prefix must not be empty when network.mode is host-tap")
		}
	}
	if cfg.Network.NATBackend == "" {
		return errors.New("network.nat_backend must not be empty")
	}
	if cfg.Network.NATBackend != "auto" && cfg.Network.NATBackend != "iptables" && cfg.Network.NATBackend != "nft" && cfg.Network.NATBackend != "none" {
		return fmt.Errorf("network.nat_backend must be one of auto, iptables, nft, or none")
	}
	return nil
}
