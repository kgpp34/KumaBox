package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newDebugCommand(opts *rootOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "debug",
		Short: "Inspect plans without changing host state",
	}
	command.AddCommand(newDebugLaunchCommand(opts))
	return command
}

func newDebugLaunchCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{name: "launch-preview", cpus: 1, memory: "512M", networks: []string{"none"}}
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "launch IMAGE",
		Short: "Render a VM launch plan without creating it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !jsonOutput {
				return errors.New("debug launch requires --json")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			request, err := newCreateRequest(flags, args, cfg)
			if err != nil {
				return err
			}
			record, err := vmstore.PreviewRecord(request, cfg.Runtime.RootDir, "kb_preview")
			if err != nil {
				return err
			}
			launch := cloudhypervisor.NewConfig(cfg, record)
			if err := cloudhypervisor.ValidateConfig(launch); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), struct {
				SchemaVersion string                 `json:"schemaVersion"`
				DryRun        bool                   `json:"dryRun"`
				VM            *vmstore.VMRecord      `json:"vm"`
				Launch        cloudhypervisor.Config `json:"launch"`
			}{
				SchemaVersion: "kumabox.debug.launch.v1",
				DryRun:        true,
				VM:            record,
				Launch:        launch,
			})
		},
	}
	command.Flags().StringVar(&flags.name, "name", flags.name, "preview VM name")
	command.Flags().IntVar(&flags.cpus, "cpus", flags.cpus, "number of vCPUs")
	command.Flags().StringVar(&flags.memory, "memory", flags.memory, "guest memory size")
	command.Flags().StringVar(&flags.storage, "storage", "", "per-VM writable COW size")
	command.Flags().StringArrayVar(&flags.dataDisks, "data-disk", nil, "managed data disk")
	command.Flags().BoolVar(&flags.sharedMemory, "shared-memory", false, "enable shared guest memory")
	command.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return command
}
