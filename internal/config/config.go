package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

const (
	defaultRootDir = "/var/lib/kumabox"
	defaultRunDir  = "/run/kumabox"
	defaultLogDir  = "/var/log/kumabox"
)

type Config struct {
	Runtime RuntimeConfig `toml:"runtime" json:"runtime"`
	Backend BackendConfig `toml:"backend" json:"backend"`
	Network NetworkConfig `toml:"network" json:"network"`
}

// RuntimeConfig contains host paths used for persistent state and runtime files.
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
}

// Load reads config from path, applies overrides, and validates the result.
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
func Default() Config {
	return Config{
		Runtime: RuntimeConfig{
			RootDir: defaultRootDir,
			RunDir:  defaultRunDir,
			LogDir:  defaultLogDir,
		},
		Backend: BackendConfig{
			CloudHypervisor: CloudHypervisorConfig{
				Binary:             "cloud-hypervisor",
				APISocketTimeoutMS: 5000,
				StopTimeoutMS:      10000,
			},
		},
		Network: NetworkConfig{
			Mode:         "host-tap",
			Default:      "default",
			Bridge:       "kumabox0",
			CIDR:         "10.88.0.0/16",
			Gateway:      "10.88.0.1",
			DNS:          []string{"1.1.1.1", "8.8.8.8"},
			TapPrefix:    "kbtap",
			NATBackend:   "auto",
			CNIConfigDir: "/etc/cni/net.d",
			CNIBinDir:    "/opt/cni/bin",
		},
	}
}

// EnsureRuntimeDirs creates the configured runtime directories.
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
