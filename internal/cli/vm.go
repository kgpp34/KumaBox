package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newCreateCommand(opts *rootOptions) *cobra.Command {
	var name string
	var rootDisk string
	var kernel string
	var initrd string

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

			store := vmstore.New(cfg.Runtime.RootDir)
			rec, err := store.Create(vmstore.CreateRequest{
				Name:     name,
				RootDisk: rootDisk,
				Kernel:   kernel,
				Initrd:   initrd,
				RunDir:   cfg.Runtime.RunDir,
				LogDir:   cfg.Runtime.LogDir,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "VM name")
	cmd.Flags().StringVar(&rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&initrd, "initrd", "", "initrd image path")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("root-disk")
	_ = cmd.MarkFlagRequired("kernel")
	_ = cmd.MarkFlagRequired("initrd")
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
