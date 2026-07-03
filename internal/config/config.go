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
	return nil
}
