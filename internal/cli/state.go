package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newPauseCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pause VM",
		Short: "Pause a running VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rec, err := kbruntime.New(cfg).PauseVM(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
}

func newResumeCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "resume VM",
		Short: "Resume a paused VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rec, err := kbruntime.New(cfg).ResumeVM(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
}
