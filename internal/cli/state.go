package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/vm/runtime"
)

func newPauseCommand(opts *rootOptions) *cobra.Command {
	var concurrency int
	cmd := &cobra.Command{
		Use:   "pause VM [VM...]",
		Short: "Pause one or more running VMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			batchOpts, err := lifecycleBatchOptions(concurrency)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			result := rt.PauseVMs(cmd.Context(), args, batchOpts)
			return writeLifecycleBatchResult(cmd, args, "pause", result)
		},
	}
	addBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}

func newResumeCommand(opts *rootOptions) *cobra.Command {
	var concurrency int
	cmd := &cobra.Command{
		Use:   "resume VM [VM...]",
		Short: "Resume one or more paused VMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			batchOpts, err := lifecycleBatchOptions(concurrency)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			result := rt.ResumeVMs(cmd.Context(), args, batchOpts)
			return writeLifecycleBatchResult(cmd, args, "resume", result)
		},
	}
	addBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}
