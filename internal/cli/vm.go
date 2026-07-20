package cli

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	defaultOCIStorageSize = "4G"
	defaultOCIMemorySize  = "512M"
)

func errInvalidLogSource(source string) error {
	return fmt.Errorf("invalid log source %q: expected console, stdout, stderr, vmm, or all", source)
}

func newCreateCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{}

	cmd := &cobra.Command{
		Use:   "create [IMAGE]",
		Short: "Create a VM record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt := kbruntime.New(cfg)
			req, err := newCreateRequest(flags, args, cfg)
			if err != nil {
				return err
			}
			rec, err := rt.CreateVM(req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	addCreateVMFlags(cmd, &flags)
	return cmd
}

func newRunCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{}
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "run [IMAGE]",
		Short: "Create and start a VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt := kbruntime.New(cfg)
			req, err := newCreateRequest(flags, args, cfg)
			if err != nil {
				return err
			}
			runContext := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				runContext, cancel = context.WithTimeout(runContext, timeout)
				defer cancel()
			}
			rec, err := rt.RunVMContext(runContext, req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	addCreateVMFlags(cmd, &flags)
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "VM startup and guest readiness timeout")
	return cmd
}

func newStartCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start VM",
		Short: "Start a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt := kbruntime.New(cfg)
			rec, err := rt.StartVMContext(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	return cmd
}

func newStopCommand(opts *rootOptions) *cobra.Command {
	var timeout time.Duration
	var force bool

	cmd := &cobra.Command{
		Use:   "stop VM",
		Short: "Stop a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if timeout <= 0 && cfg.Backend.CloudHypervisor.StopTimeoutMS > 0 {
				timeout = time.Duration(cfg.Backend.CloudHypervisor.StopTimeoutMS) * time.Millisecond
			}
			rt := kbruntime.New(cfg)
			rec, err := rt.StopVMContext(cmd.Context(), args[0], backend.StopOptions{
				Timeout: timeout,
				Force:   force,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 0, "graceful shutdown timeout")
	cmd.Flags().BoolVar(&force, "force", false, "skip API shutdown and terminate the VMM")
	return cmd
}

func newInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "inspect VM",
		Short: "Inspect a VM record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt := kbruntime.New(cfg)
			rec, err := rt.InspectVM(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeVMTable(cmd.OutOrStdout(), []*vmstore.VMRecord{rec})
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newLogsCommand(opts *rootOptions) *cobra.Command {
	var tail int
	var source string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "logs VM",
		Short: "Show VM logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if !kbruntime.ValidLogSource(source) {
				return errInvalidLogSource(source)
			}
			rt := kbruntime.New(cfg)
			logs, err := rt.LogsVM(args[0], kbruntime.LogOptions{
				Tail:   tail,
				Source: source,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), logs)
			}
			return writeVMLogs(cmd.OutOrStdout(), logs)
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 100, "number of recent lines to show, 0 for all")
	cmd.Flags().StringVar(&source, "source", kbruntime.LogSourceConsole, "log source: console, stdout, stderr, vmm, all")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newDeleteCommand(opts *rootOptions) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete VM",
		Short: "Delete a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt := kbruntime.New(cfg)
			rec, err := rt.DeleteVMContext(cmd.Context(), args[0], force)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop running VM before deleting it")
	return cmd
}

type createVMFlags struct {
	name     string
	rootDisk string
	kernel   string
	initrd   string
	firmware string
	cpus     int
	memory   string
	storage  string
	networks []string
}

func addCreateVMFlags(cmd *cobra.Command, flags *createVMFlags) {
	cmd.Flags().StringVar(&flags.name, "name", "", "VM name")
	cmd.Flags().StringVar(&flags.rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&flags.kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&flags.initrd, "initrd", "", "initrd image path")
	cmd.Flags().StringVar(&flags.firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().IntVar(&flags.cpus, "cpus", 1, "number of vCPUs")
	cmd.Flags().StringVar(&flags.memory, "memory", "512M", "guest memory size, for example 512M or 2G")
	cmd.Flags().StringVar(&flags.storage, "storage", "", "per-VM writable COW size for OCI images, for example 4G")
	cmd.Flags().StringArrayVar(&flags.networks, "network", nil, "network attachment, repeatable: none, default, host-tap, cni, or cni:NAME")
	_ = cmd.MarkFlagRequired("name")
}

func newCreateRequest(flags createVMFlags, args []string, cfg config.Config) (vmstore.CreateRequest, error) {
	if flags.cpus < 0 {
		return vmstore.CreateRequest{}, fmt.Errorf("--cpus must be greater than zero")
	}
	if flags.cpus == 0 {
		flags.cpus = 1
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, "512M"))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	if len(args) == 0 {
		if flags.rootDisk == "" {
			return vmstore.CreateRequest{}, fmt.Errorf("either IMAGE or --root-disk is required")
		}
		return vmstore.CreateRequest{
			Name:        flags.name,
			RootDisk:    flags.rootDisk,
			Kernel:      flags.kernel,
			Initrd:      flags.initrd,
			Firmware:    flags.firmware,
			CPUs:        flags.cpus,
			MemoryBytes: memoryBytes,
			Networks:    normalizedNetworkFlags(flags.networks),
			RunDir:      cfg.Runtime.RunDir,
			LogDir:      cfg.Runtime.LogDir,
		}, nil
	}

	if flags.rootDisk != "" || flags.kernel != "" || flags.initrd != "" || flags.firmware != "" {
		return vmstore.CreateRequest{}, fmt.Errorf("IMAGE cannot be combined with --root-disk, --kernel, --initrd, or --firmware")
	}
	image, err := imagestore.New(cfg.Runtime.RootDir).Inspect(args[0])
	if err != nil {
		return vmstore.CreateRequest{}, fmt.Errorf("resolve image %q: %w", args[0], err)
	}
	if image.OCI == nil && image.RootDisk.Path == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no root disk", args[0])
	}
	if image.OCI != nil {
		return newOCIImageCreateRequest(flags, image, cfg)
	}
	if image.RootDisk.Format != vmstore.FormatQCOW2 {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q root disk format %q cannot use a qcow2 overlay", image.Name, image.RootDisk.Format)
	}
	if image.RootDisk.SHA256 == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q root disk has no pinned sha256 digest", image.Name)
	}
	digest := image.RootDisk.SHA256
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	req := vmstore.CreateRequest{
		Name:        flags.name,
		RootDisk:    image.RootDisk.Path,
		Kernel:      image.Boot.Kernel,
		Initrd:      image.Boot.Initrd,
		Firmware:    image.Boot.Firmware,
		CPUs:        flags.cpus,
		MemoryBytes: memoryBytes,
		Networks:    normalizedNetworkFlags(flags.networks),
		Image: &vmstore.ImageRef{
			ID:       image.ID,
			Name:     image.Name,
			RootDisk: image.RootDisk.Path,
			BootMode: image.Boot.Mode,
			Digest:   digest,
		},
		StorageConfigs: []vmstore.StorageConfig{{
			ID:               "root",
			Role:             vmstore.StorageRoleCOW,
			Format:           vmstore.FormatQCOW2,
			VirtualSizeBytes: image.RootDisk.VirtualSizeBytes,
			Base: &vmstore.StorageBase{
				Family:  "cloudimg",
				ImageID: image.ID,
				Digest:  digest,
				Format:  image.RootDisk.Format,
				Path:    image.RootDisk.Path,
			},
		}},
		RunDir: cfg.Runtime.RunDir,
		LogDir: cfg.Runtime.LogDir,
	}
	if req.Firmware == "" && (req.Kernel == "" || req.Initrd == "") {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no usable boot configuration", args[0])
	}
	return req, nil
}

func newOCIImageCreateRequest(flags createVMFlags, image *imagestore.ImageRecord, cfg config.Config) (vmstore.CreateRequest, error) {
	if image.Boot.Mode != "direct" || image.Boot.Kernel == "" || image.Boot.Initrd == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no OCI direct boot profile", image.Name)
	}
	cowSize, err := parseByteSize(defaultString(flags.storage, defaultOCIStorageSize))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, defaultOCIMemorySize))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	manifestDigest := image.OCI.DigestRef
	if _, digest, found := strings.Cut(manifestDigest, "@"); found {
		manifestDigest = digest
	}
	if manifestDigest == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no OCI manifest digest", image.Name)
	}
	storageConfigs := make([]vmstore.StorageConfig, 0, len(image.OCI.Layers)+1)
	layerDigests := make([]string, 0, len(image.OCI.Layers))
	for i, layer := range image.OCI.Layers {
		if layer.EROFS == nil || layer.EROFS.Path == "" {
			return vmstore.CreateRequest{}, fmt.Errorf("image %q layer %d has no EROFS blob", image.Name, i)
		}
		serial := layer.Serial
		if serial == "" {
			serial = vmstore.LayerSerial(i)
		}
		storageConfigs = append(storageConfigs, vmstore.StorageConfig{
			ID:               vmstore.LayerID(i),
			Role:             vmstore.StorageRoleLayer,
			Path:             layer.EROFS.Path,
			Readonly:         true,
			Format:           vmstore.FormatRaw,
			Serial:           serial,
			Filesystem:       vmstore.FilesystemEROFS,
			SourceLayer:      layer.Digest,
			VirtualSizeBytes: layer.EROFS.SizeBytes,
		})
		layerDigests = append(layerDigests, layer.Digest)
	}
	storageConfigs = append(storageConfigs, vmstore.StorageConfig{
		ID:               vmstore.StorageIDCOW,
		Role:             vmstore.StorageRoleCOW,
		Readonly:         false,
		Format:           vmstore.FormatRaw,
		Serial:           vmstore.StorageSerialCOW,
		Filesystem:       vmstore.FilesystemEXT4,
		VirtualSizeBytes: cowSize,
		Base: &vmstore.StorageBase{
			Family:       vmstore.BaseFamilyOCI,
			ImageID:      image.ID,
			Digest:       manifestDigest,
			LayerDigests: append([]string(nil), layerDigests...),
		},
	})
	return vmstore.CreateRequest{
		Name:           flags.name,
		Kernel:         image.Boot.Kernel,
		Initrd:         image.Boot.Initrd,
		KernelCmdline:  image.Boot.Cmdline,
		CPUs:           flags.cpus,
		MemoryBytes:    memoryBytes,
		Networks:       normalizedOCIImageNetworkFlags(flags.networks, cfg),
		StorageConfigs: storageConfigs,
		Image: &vmstore.ImageRef{
			ID:           image.ID,
			Name:         image.Name,
			RootDisk:     image.RootDisk.Path,
			BootMode:     image.Boot.Mode,
			Digest:       manifestDigest,
			LayerDigests: append([]string(nil), layerDigests...),
		},
		RunDir: cfg.Runtime.RunDir,
		LogDir: cfg.Runtime.LogDir,
	}, nil
}

func parseByteSize(value string) (int64, error) {
	return parsePositiveByteSize("--storage", value)
}

func parseMemorySize(value string) (int64, error) {
	return parsePositiveByteSize("--memory", value)
}

func parsePositiveByteSize(flag, value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s must not be empty", flag)
	}
	multiplier := int64(1)
	suffix := strings.ToUpper(trimmed[len(trimmed)-1:])
	switch suffix {
	case "K":
		multiplier = 1024
		trimmed = trimmed[:len(trimmed)-1]
	case "M":
		multiplier = 1024 * 1024
		trimmed = trimmed[:len(trimmed)-1]
	case "G":
		multiplier = 1024 * 1024 * 1024
		trimmed = trimmed[:len(trimmed)-1]
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive size like 512M or 4G", flag)
	}
	if n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%s exceeds the supported size", flag)
	}
	return n * multiplier, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func normalizedNetworkFlags(values []string) []string {
	if len(values) == 0 {
		return []string{"none"}
	}
	return append([]string(nil), values...)
}

func normalizedOCIImageNetworkFlags(values []string, cfg config.Config) []string {
	if len(values) > 0 {
		return append([]string(nil), values...)
	}
	if cfg.Network.Mode == kbnetwork.ProviderCNI {
		return []string{"cni:" + cfg.Network.Default}
	}
	return []string{"none"}
}

func newPSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List VM records",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt := kbruntime.New(cfg)
			records, err := rt.ListVMs()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeVMTable(cmd.OutOrStdout(), records)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
