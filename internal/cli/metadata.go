package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/lock"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	"github.com/kumabox/kumabox/internal/state"
)

func newMetadataCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "metadata", Short: "Inspect metadata storage"}
	cmd.AddCommand(newMetadataInitCommand(opts))
	cmd.AddCommand(newMetadataStatusCommand(opts))
	cmd.AddCommand(newMetadataVerifyCommand(opts))
	cmd.AddCommand(newMetadataConvertCommand(opts))
	cmd.AddCommand(newMetadataBackupCommand(opts))
	return cmd
}

func newMetadataBackupCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "backup OUTPUT", Short: "Create a verified SQLite metadata backup", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if cfg.Metadata.Backend != "sqlite" {
				return fmt.Errorf("metadata backup requires the sqlite backend, got %q", cfg.Metadata.Backend)
			}
			destination, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve metadata backup destination: %w", err)
			}
			if err := metasqlite.Backup(cmd.Context(), state.SQLiteMetadataPath(cfg), destination); err != nil {
				return err
			}
			info, err := os.Stat(destination)
			if err != nil {
				return fmt.Errorf("stat metadata backup: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"backend": "sqlite", "output": destination, "sizeBytes": info.Size(), "verified": true,
			})
		},
	}
}

func newMetadataInitCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "init", Short: "Initialize the configured SQLite metadata database", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := state.InitSQLiteMetadata(cmd.Context(), cfg); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"backend": "sqlite", "path": state.SQLiteMetadataPath(cfg), "initialized": true,
			})
		},
	}
}

func newMetadataConvertCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "convert", Short: "Switch metadata to the configured backend", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			maintenance, err := lock.NewGuard(cfg.Runtime.RootDir).BeginMaintenance(cmd.Context())
			if err != nil {
				return err
			}
			defer maintenance.Release() //nolint:errcheck
			result, err := state.ConvertMetadata(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
}

func newMetadataStatusCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show metadata backend and namespace state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			result := map[string]any{"backend": cfg.Metadata.Backend}
			if cfg.Metadata.Backend == "sqlite" {
				path := state.SQLiteMetadataPath(cfg)
				result["path"] = path
				engine, ok := stores.Metadata.(*metasqlite.Store)
				if !ok {
					return fmt.Errorf("configured SQLite metadata engine has unexpected type %T", stores.Metadata)
				}
				status, err := engine.Status(cmd.Context())
				if err != nil {
					return err
				}
				result["namespaces"] = status
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
}

func newMetadataVerifyCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "verify", Short: "Verify metadata backend identity and namespace state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if cfg.Metadata.Backend != "sqlite" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"backend": cfg.Metadata.Backend, "verified": true})
			}
			engine, ok := stores.Metadata.(*metasqlite.Store)
			if !ok {
				return fmt.Errorf("configured SQLite metadata engine has unexpected type %T", stores.Metadata)
			}
			status, err := engine.Status(cmd.Context())
			if err != nil {
				return err
			}
			for _, namespace := range status {
				if namespace.State != "initialized" && namespace.State != "converted" {
					return fmt.Errorf("metadata namespace %q has invalid state %q", namespace.Namespace, namespace.State)
				}
			}
			if err := engine.Verify(cmd.Context()); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{"backend": "sqlite", "verified": true, "namespaces": status})
		},
	}
}
