package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

func newNetworkCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect VM network resources",
	}
	cmd.AddCommand(newNetworkLSCommand(opts))
	cmd.AddCommand(newNetworkInspectCommand(opts))
	cmd.AddCommand(newNetworkSetupCommand(opts))
	cmd.AddCommand(newNetworkTeardownCommand(opts))
	return cmd
}

func newNetworkLSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List network provider records",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			records, err := kbnetwork.NewStore(cfg.Runtime.RootDir).List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeNetworkTable(cmd.OutOrStdout(), records)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newNetworkSetupCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Ensure the default host-tap network",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			report, err := kbnetwork.EnsureHostTap(ctx, cfg.Runtime.RootDir, cfg.Network)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), report)
			}
			return writeJSON(cmd.OutOrStdout(), report)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newNetworkTeardownCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove the default host-tap network if owned by this root dir",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			report, err := kbnetwork.TeardownHostTap(ctx, cfg.Runtime.RootDir, cfg.Network)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), report)
			}
			return writeJSON(cmd.OutOrStdout(), report)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newNetworkInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "inspect VM",
		Short: "Inspect one VM's network provider records",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			result, err := kbnetwork.NewStore(cfg.Runtime.RootDir).Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			return writeNetworkTable(cmd.OutOrStdout(), result.Interfaces)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
