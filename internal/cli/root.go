package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
)

type rootOptions struct {
	configPath         string
	configOverride     *config.Config
	cloudHypervisorBin string
	qemuImgBin         string
	metadataBackend    string
	metadataPath       string
}

func NewRootCommand() *cobra.Command {
	opts := &rootOptions{}
	return newRootCommand(opts)
}

// NewRootCommandWithConfig creates a command with an injected configuration.
// It is intended for embedding and tests that need isolated storage roots.
func NewRootCommandWithConfig(cfg config.Config) *cobra.Command {
	return newRootCommand(&rootOptions{configOverride: &cfg})
}

func newRootCommand(opts *rootOptions) *cobra.Command {

	cmd := &cobra.Command{
		Use:           "kumabox",
		Short:         "KumaBox microVM sandbox runtime",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&opts.configPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&opts.cloudHypervisorBin, "cloud-hypervisor-bin", "", "cloud-hypervisor binary path")
	cmd.PersistentFlags().StringVar(&opts.qemuImgBin, "qemu-img-bin", "", "qemu-img binary path")
	cmd.PersistentFlags().StringVar(&opts.metadataBackend, "metadata-backend", "", "metadata backend: json or sqlite")
	cmd.PersistentFlags().StringVar(&opts.metadataPath, "metadata-path", "", "SQLite metadata database path")

	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newDoctorCommand(opts))
	cmd.AddCommand(newCreateCommand(opts))
	cmd.AddCommand(newRunCommand(opts))
	cmd.AddCommand(newStartCommand(opts))
	cmd.AddCommand(newStopCommand(opts))
	cmd.AddCommand(newPauseCommand(opts))
	cmd.AddCommand(newResumeCommand(opts))
	cmd.AddCommand(newRestoreCommand(opts))
	cmd.AddCommand(newCloneCommand(opts))
	cmd.AddCommand(newHibernateCommand(opts))
	cmd.AddCommand(newInspectCommand(opts))
	cmd.AddCommand(newLogsCommand(opts))
	cmd.AddCommand(newConsoleCommand(opts))
	cmd.AddCommand(newDeleteCommand(opts))
	cmd.AddCommand(newGCCommand(opts))
	cmd.AddCommand(newImageCommand(opts))
	cmd.AddCommand(newSnapshotCommand(opts))
	cmd.AddCommand(newNetworkCommand(opts))
	cmd.AddCommand(newDiskCommand(opts))
	cmd.AddCommand(newFilesystemCommand(opts))
	cmd.AddCommand(newPCIDeviceCommand(opts))
	cmd.AddCommand(newAgentCommand(opts))
	cmd.AddCommand(newExecCommand(opts))
	cmd.AddCommand(newPSCommand(opts))
	cmd.AddCommand(newMetadataCommand(opts))
	return cmd
}

func loadConfig(opts *rootOptions) (config.Config, error) {
	if opts.configOverride != nil {
		cfg := *opts.configOverride
		if opts.cloudHypervisorBin != "" {
			cfg.Backend.CloudHypervisor.Binary = opts.cloudHypervisorBin
		}
		if opts.qemuImgBin != "" {
			cfg.Storage.QEMUImgBinary = opts.qemuImgBin
		}
		if opts.metadataBackend != "" {
			cfg.Metadata.Backend = opts.metadataBackend
		}
		if opts.metadataPath != "" {
			cfg.Metadata.Path = opts.metadataPath
		}
		return cfg, nil
	}
	overrides := config.Overrides{
		CloudHypervisorBin: opts.cloudHypervisorBin,
		QEMUImgBinary:      opts.qemuImgBin,
		MetadataBackend:    opts.metadataBackend,
		MetadataPath:       opts.metadataPath,
	}
	return config.Load(opts.configPath, overrides)
}
