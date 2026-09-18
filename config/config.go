// Package config loads and validates one immutable application configuration
// snapshot for each KumaBox invocation. Modules receive their own options from
// core and never read this package or the environment directly.
package config

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const environmentPrefix = "KUMABOX"

// Config is the validated application configuration shared by one command.
type Config struct {
	// Paths contains the three non-overlapping host ownership roots.
	Paths storage.Roots `mapstructure:"paths"`
	// Images controls image conversion tools, concurrency, and input bounds.
	Images Images `mapstructure:"images"`
	// Metadata controls bounded SQLite lock and transaction waits.
	Metadata Metadata `mapstructure:"metadata"`
	// Sandbox controls writable disk preparation and compensation.
	Sandbox Sandbox `mapstructure:"sandbox"`
	// VMM selects and configures process backends.
	VMM VMM `mapstructure:"vmm"`
}

// Images contains operator-controlled image import limits.
type Images struct {
	// EROFSBinary is the mkfs.erofs executable name or absolute path.
	EROFSBinary string `mapstructure:"erofs_binary"`
	// Parallelism bounds concurrent layer reuse checks and conversions.
	Parallelism int `mapstructure:"parallelism"`
	// LayerSize caps one compressed source layer in bytes.
	LayerSize int64 `mapstructure:"layer_size"`
	// UnpackedSize caps one decompressed layer tar stream in bytes.
	UnpackedSize int64 `mapstructure:"unpacked_size"`
	// BootSize caps one extracted kernel or initrd in bytes.
	BootSize int64 `mapstructure:"boot_size"`
	// ArchiveSize caps expanded regular-file content in a local archive.
	ArchiveSize int64 `mapstructure:"archive_size"`
}

// Metadata contains SQLite wait budgets.
type Metadata struct {
	// BusyTimeout is one SQLite busy-handler wait.
	BusyTimeout time.Duration `mapstructure:"busy_timeout"`
	// RetryLimit bounds writer acquisition and transaction execution.
	RetryLimit time.Duration `mapstructure:"retry_limit"`
}

// Sandbox contains host disk and compensation policy.
type Sandbox struct {
	// Ext4Binary is the mkfs.ext4 executable name or absolute path.
	Ext4Binary string `mapstructure:"ext4_binary"`
	// CleanupTimeout bounds failure compensation after caller cancellation.
	CleanupTimeout time.Duration `mapstructure:"cleanup_timeout"`
}

// VMM contains backend selection and host process policy.
type VMM struct {
	// Default selects the backend for newly created sandboxes.
	Default types.VMMType `mapstructure:"default"`
	// CgroupParent contains per-sandbox VMM scopes.
	CgroupParent string `mapstructure:"cgroup_parent"`
	// CloudHypervisor configures the Cloud Hypervisor adapter.
	CloudHypervisor CloudHypervisor `mapstructure:"cloud_hypervisor"`
}

// CloudHypervisor contains executable and bounded lifecycle waits.
type CloudHypervisor struct {
	// Binary is the cloud-hypervisor executable name or absolute path.
	Binary string `mapstructure:"binary"`
	// StartupTimeout bounds process and API readiness.
	StartupTimeout time.Duration `mapstructure:"startup_timeout"`
	// StopGrace bounds the identity-checked SIGTERM to SIGKILL window.
	StopGrace time.Duration `mapstructure:"stop_grace"`
	// AbortGrace bounds failed-launch process termination.
	AbortGrace time.Duration `mapstructure:"abort_grace"`
}

// Default returns the operational defaults used when no higher-precedence
// source supplies a value.
func Default() Config {
	return Config{
		Paths: storage.DefaultRoots(),
		Images: Images{
			EROFSBinary: "mkfs.erofs", Parallelism: min(4, max(1, runtime.NumCPU())),
			LayerSize: 8 << 30, UnpackedSize: 16 << 30, BootSize: 512 << 20, ArchiveSize: 32 << 30,
		},
		Metadata: Metadata{BusyTimeout: 50 * time.Millisecond, RetryLimit: 5 * time.Second},
		Sandbox:  Sandbox{Ext4Binary: "mkfs.ext4", CleanupTimeout: 10 * time.Second},
		VMM: VMM{
			Default: types.VMMCloudHypervisor, CgroupParent: "/sys/fs/cgroup/kumabox.slice",
			CloudHypervisor: CloudHypervisor{
				Binary: "cloud-hypervisor", StartupTimeout: 10 * time.Second,
				StopGrace: 5 * time.Second, AbortGrace: 3 * time.Second,
			},
		},
	}
}

// Validate normalizes roots and rejects incomplete or unbounded policy before
// any module creates host resources.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is required")
	}
	paths, err := c.Paths.Validate()
	if err != nil {
		return fmt.Errorf("paths: %w", err)
	}
	c.Paths = paths
	if strings.TrimSpace(c.Images.EROFSBinary) == "" || c.Images.Parallelism <= 0 ||
		!validSize(c.Images.LayerSize) || !validSize(c.Images.UnpackedSize) ||
		!validSize(c.Images.BootSize) || !validSize(c.Images.ArchiveSize) {
		return errors.New("images requires an EROFS binary, positive parallelism, and positive bounded size limits")
	}
	if c.Metadata.BusyTimeout <= 0 || c.Metadata.RetryLimit <= 0 {
		return errors.New("metadata timeouts must be positive")
	}
	if strings.TrimSpace(c.Sandbox.Ext4Binary) == "" || c.Sandbox.CleanupTimeout <= 0 {
		return errors.New("sandbox requires an ext4 binary and positive cleanup timeout")
	}
	if err := c.VMM.Default.Validate(); err != nil {
		return fmt.Errorf("vmm default: %w", err)
	}
	if strings.TrimSpace(c.VMM.CgroupParent) == "" {
		return errors.New("vmm cgroup parent must not be empty")
	}
	cloudHypervisor := c.VMM.CloudHypervisor
	if strings.TrimSpace(cloudHypervisor.Binary) == "" || cloudHypervisor.StartupTimeout <= 0 ||
		cloudHypervisor.StopGrace <= 0 || cloudHypervisor.AbortGrace <= 0 {
		return errors.New("cloud hypervisor binary and lifecycle timeouts must be positive")
	}
	return nil
}

func validSize(value int64) bool { return value > 0 && value < 1<<63-1 }

// Loader resolves defaults, one explicit file, environment variables, and
// bound flags using an invocation-local Viper instance.
type Loader struct {
	resolver *viper.Viper
}

// NewLoader creates an isolated loader. It never searches implicit config
// locations, so privileged commands cannot consume an unrelated working-tree file.
func NewLoader() *Loader {
	resolver := viper.New()
	resolver.SetEnvPrefix(environmentPrefix)
	resolver.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	resolver.AutomaticEnv()
	defaults := Default()
	for key, value := range map[string]any{
		"paths.data": defaults.Paths.Data, "paths.run": defaults.Paths.Run, "paths.log": defaults.Paths.Log,
		"images.erofs_binary": defaults.Images.EROFSBinary, "images.parallelism": defaults.Images.Parallelism,
		"images.layer_size": defaults.Images.LayerSize, "images.unpacked_size": defaults.Images.UnpackedSize,
		"images.boot_size": defaults.Images.BootSize, "images.archive_size": defaults.Images.ArchiveSize,
		"metadata.busy_timeout": defaults.Metadata.BusyTimeout, "metadata.retry_limit": defaults.Metadata.RetryLimit,
		"sandbox.ext4_binary": defaults.Sandbox.Ext4Binary, "sandbox.cleanup_timeout": defaults.Sandbox.CleanupTimeout,
		"vmm.default": defaults.VMM.Default, "vmm.cgroup_parent": defaults.VMM.CgroupParent,
		"vmm.cloud_hypervisor.binary":          defaults.VMM.CloudHypervisor.Binary,
		"vmm.cloud_hypervisor.startup_timeout": defaults.VMM.CloudHypervisor.StartupTimeout,
		"vmm.cloud_hypervisor.stop_grace":      defaults.VMM.CloudHypervisor.StopGrace,
		"vmm.cloud_hypervisor.abort_grace":     defaults.VMM.CloudHypervisor.AbortGrace,
	} {
		resolver.SetDefault(key, value)
	}
	return &Loader{resolver: resolver}
}

// BindFlag gives one Cobra flag precedence over environment, file, and default
// values. Bindings must be completed before Load is called.
func (l *Loader) BindFlag(key string, flag *pflag.Flag) error {
	if l == nil || l.resolver == nil {
		return errors.New("config loader is not initialized")
	}
	if flag == nil {
		return fmt.Errorf("bind config key %q: flag is missing", key)
	}
	return l.resolver.BindPFlag(key, flag)
}

// Load reads one explicitly requested config file and resolves all registered
// sources. An empty path deliberately skips filesystem config discovery.
func (l *Loader) Load(path string) (Config, error) {
	if l == nil || l.resolver == nil {
		return Config{}, errors.New("config loader is not initialized")
	}
	if path != "" {
		l.resolver.SetConfigFile(path)
		if err := l.resolver.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	var result Config
	if err := l.resolver.UnmarshalExact(&result); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := result.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	return result, nil
}
