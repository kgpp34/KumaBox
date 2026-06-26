package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newCreateCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{}

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a VM record",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt := kbruntime.New(cfg)
			rec, err := rt.CreateVM(newCreateRequest(flags, cfg))
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
		Use:   "run",
		Short: "Create and start a VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt := kbruntime.New(cfg)
			rec, err := rt.RunVM(newCreateRequest(flags, cfg))
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
			rec, err := vmstore.New(cfg.Runtime.RootDir).Inspect(args[0])
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

type createVMFlags struct {
	name     string
	rootDisk string
	kernel   string
	initrd   string
}

func addCreateVMFlags(cmd *cobra.Command, flags *createVMFlags) {
	cmd.Flags().StringVar(&flags.name, "name", "", "VM name")
	cmd.Flags().StringVar(&flags.rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&flags.kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&flags.initrd, "initrd", "", "initrd image path")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("root-disk")
	_ = cmd.MarkFlagRequired("kernel")
	_ = cmd.MarkFlagRequired("initrd")
}

func newCreateRequest(flags createVMFlags, cfg config.Config) vmstore.CreateRequest {
	return vmstore.CreateRequest{
		Name:     flags.name,
		RootDisk: flags.rootDisk,
		Kernel:   flags.kernel,
		Initrd:   flags.initrd,
		RunDir:   cfg.Runtime.RunDir,
		LogDir:   cfg.Runtime.LogDir,
	}
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
			records, err := vmstore.New(cfg.Runtime.RootDir).List()
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
