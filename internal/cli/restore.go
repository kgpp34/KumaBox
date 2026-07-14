package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newRestoreCommand(opts *rootOptions) *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "restore VM SNAPSHOT",
		Short: "Restore a native running snapshot into its original VM",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rec, err := kbruntime.New(cfg).RestoreNativeVM(cmd.Context(), args[0], args[1], kbruntime.NativeRestoreOptions{Mode: mode})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&mode, "restore-mode", "copy", "memory restore mode: copy")
	return cmd
}
