package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newCloneCommand(opts *rootOptions) *cobra.Command {
	var name, mode string
	var networks []string
	cmd := &cobra.Command{
		Use:   "clone SNAPSHOT",
		Short: "Create a running VM with new identity from a native snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rec, err := kbruntime.New(cfg).CloneNativeSnapshot(cmd.Context(), args[0], kbruntime.NativeCloneOptions{
				Name: name, Networks: networks, Mode: mode,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "new VM name")
	cmd.Flags().StringArrayVar(&networks, "network", nil, "new network attachment, repeatable")
	cmd.Flags().StringVar(&mode, "restore-mode", "copy", "memory restore mode: copy, ondemand, or mmap")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
