package cli

import (
	"fmt"
	"strings"

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
	cmd.AddCommand(newUsageCommand(opts))
	cmd.AddCommand(newMetadataCommand(opts))
	cmd.AddCommand(newDebugCommand(opts))
	cmd.AddCommand(newCompletionCommand())
	configureResourceCompletions(cmd, opts)
	return cmd
}

func newCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletion(cmd.OutOrStdout())
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}

func configureResourceCompletions(root *cobra.Command, opts *rootOptions) {
	for _, command := range root.Commands() {
		configureResourceCompletions(command, opts)
		if _, ok := commandResourceKind(command.Use, 0); command.ValidArgsFunction == nil && ok {
			command.ValidArgsFunction = completeResources(opts)
		}
	}
}

func commandResourceKind(use string, argIndex int) (string, bool) {
	fields := strings.Fields(use)
	if len(fields) < 2 {
		return "", false
	}
	arguments := fields[1:]
	if argIndex >= len(arguments) {
		last := strings.Trim(arguments[len(arguments)-1], "[]")
		if !strings.HasSuffix(last, "...") {
			return "", false
		}
		argIndex = len(arguments) - 1
	}
	field := strings.Trim(arguments[argIndex], "[]")
	field = strings.TrimSuffix(field, "...")
	switch field {
	case "VM", "IMAGE", "SNAPSHOT":
		return strings.ToLower(field), true
	default:
		return "", false
	}
}

func completeResources(opts *rootOptions) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		kind, ok := commandResourceKind(cmd.Use, len(args))
		if !ok {
			return nil, cobra.ShellCompDirectiveDefault
		}
		cfg, err := loadConfig(opts)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		stores, err := configuredStores(cfg)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		if stores.Metadata != nil {
			defer func() { _ = stores.Metadata.Close() }()
		}
		var candidates []string
		switch kind {
		case "vm":
			records, listErr := stores.VM.List()
			if listErr != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			for _, record := range records {
				candidates = append(candidates, record.Name)
			}
		case "image":
			records, listErr := stores.Images.List()
			if listErr != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			for _, record := range records {
				candidates = append(candidates, record.Name)
			}
		case "snapshot":
			records, listErr := stores.Snapshots.List()
			if listErr != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			for _, record := range records {
				candidates = append(candidates, record.Name)
			}
		}
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if strings.HasPrefix(candidate, toComplete) {
				filtered = append(filtered, candidate)
			}
		}
		return filtered, cobra.ShellCompDirectiveNoFileComp
	}
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
