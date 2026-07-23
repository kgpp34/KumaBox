package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
)

func newMetadataCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "metadata", Short: "Inspect metadata storage"}
	cmd.AddCommand(newMetadataStatusCommand(opts))
	return cmd
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
