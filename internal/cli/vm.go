package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
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
			rec, err := rt.RunVM(req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	addCreateVMFlags(cmd, &flags)
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
			rec, err := rt.StartVM(args[0])
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
			rec, err := rt.StopVM(args[0], backend.StopOptions{
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
			rec, err := rt.DeleteVM(args[0], force)
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
	network  string
}

func addCreateVMFlags(cmd *cobra.Command, flags *createVMFlags) {
	cmd.Flags().StringVar(&flags.name, "name", "", "VM name")
	cmd.Flags().StringVar(&flags.rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&flags.kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&flags.initrd, "initrd", "", "initrd image path")
	cmd.Flags().StringVar(&flags.firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().StringVar(&flags.network, "network", "none", "network mode: none or default")
	_ = cmd.MarkFlagRequired("name")
}

func newCreateRequest(flags createVMFlags, args []string, cfg config.Config) (vmstore.CreateRequest, error) {
	if len(args) == 0 {
		if flags.rootDisk == "" {
			return vmstore.CreateRequest{}, fmt.Errorf("either IMAGE or --root-disk is required")
		}
		return vmstore.CreateRequest{
			Name:     flags.name,
			RootDisk: flags.rootDisk,
			Kernel:   flags.kernel,
			Initrd:   flags.initrd,
			Firmware: flags.firmware,
			Network:  flags.network,
			RunDir:   cfg.Runtime.RunDir,
			LogDir:   cfg.Runtime.LogDir,
		}, nil
	}

	if flags.rootDisk != "" || flags.kernel != "" || flags.initrd != "" || flags.firmware != "" {
		return vmstore.CreateRequest{}, fmt.Errorf("IMAGE cannot be combined with --root-disk, --kernel, --initrd, or --firmware")
	}
	image, err := imagestore.New(cfg.Runtime.RootDir).Inspect(args[0])
	if err != nil {
		return vmstore.CreateRequest{}, fmt.Errorf("resolve image %q: %w", args[0], err)
	}
	if image.RootDisk.Path == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no root disk", args[0])
	}
	req := vmstore.CreateRequest{
		Name:     flags.name,
		RootDisk: image.RootDisk.Path,
		Kernel:   image.Boot.Kernel,
		Initrd:   image.Boot.Initrd,
		Firmware: image.Boot.Firmware,
		Network:  flags.network,
		Image: &vmstore.ImageRef{
			ID:       image.ID,
			Name:     image.Name,
			RootDisk: image.RootDisk.Path,
			BootMode: image.Boot.Mode,
		},
		RunDir: cfg.Runtime.RunDir,
		LogDir: cfg.Runtime.LogDir,
	}
	if req.Firmware == "" && (req.Kernel == "" || req.Initrd == "") {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no usable boot configuration", args[0])
	}
	return req, nil
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
