package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	"github.com/kumabox/kumabox/internal/resources"
)

func newMetadataCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "metadata", Short: "Inspect metadata storage"}
	cmd.AddCommand(newMetadataStatusCommand(opts))
	cmd.AddCommand(newMetadataVerifyCommand(opts))
	cmd.AddCommand(newMetadataConvertCommand(opts))
	return cmd
}

func newMetadataConvertCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "convert", Short: "Convert JSON metadata into SQLite", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			path := cfg.Metadata.Path
			if path == "" {
				path = filepath.Join(cfg.Runtime.RootDir, "metadata", "kumabox.db")
			}
			status, err := resources.ConvertJSONToSQLite(cmd.Context(), cfg.Runtime.RootDir, path)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{"backend": "sqlite", "path": path, "namespaces": status})
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
				path := cfg.Metadata.Path
				if path == "" {
					path = filepath.Join(cfg.Runtime.RootDir, "metadata", "kumabox.db")
				}
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
			return writeJSON(cmd.OutOrStdout(), map[string]any{"backend": "sqlite", "verified": true, "namespaces": status})
		},
	}
}
