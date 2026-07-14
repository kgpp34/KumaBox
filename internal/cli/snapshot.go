package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/snapshot"
)

func newSnapshotCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "snapshot", Short: "Manage stopped VM snapshots"}
	cmd.AddCommand(newSnapshotCreateCommand(opts))
	cmd.AddCommand(newSnapshotExportCommand(opts))
	cmd.AddCommand(newSnapshotImportCommand(opts))
	cmd.AddCommand(newSnapshotLSCommand(opts))
	cmd.AddCommand(newSnapshotInspectCommand(opts))
	cmd.AddCommand(newSnapshotRMCommand(opts))
	return cmd
}

func newSnapshotImportCommand(opts *rootOptions) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "import PACKAGE", Short: "Import an untrusted snapshot package", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			input, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			rec, err := snapshot.NewStore(cfg.Runtime.RootDir).Import(cmd.Context(), snapshot.ImportOptions{Input: input, Name: name, QEMUImgBinary: cfg.Storage.QEMUImgBinary})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "imported snapshot name")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotExportCommand(opts *rootOptions) *cobra.Command {
	var output, compression string
	cmd := &cobra.Command{
		Use: "export SNAPSHOT", Short: "Export a portable snapshot package", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			absolute, err := filepath.Abs(output)
			if err != nil {
				return fmt.Errorf("resolve export output: %w", err)
			}
			if err := snapshot.NewStore(cfg.Runtime.RootDir).Export(cmd.Context(), args[0], snapshot.ExportOptions{Output: absolute, Compression: compression}); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]string{"snapshot": args[0], "output": absolute, "compression": compression})
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "output .kbsnap path")
	cmd.Flags().StringVar(&compression, "compression", "none", "compression: none, gzip, or zstd")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func newSnapshotCreateCommand(opts *rootOptions) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "create VM", Short: "Capture a stopped VM disk snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := kbruntime.New(cfg).CreateStoppedSnapshot(cmd.Context(), args[0], name)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "snapshot name")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotLSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List ready snapshots",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			records, err := snapshot.NewStore(cfg.Runtime.RootDir).List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeSnapshotTable(cmd.OutOrStdout(), records)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newSnapshotInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "inspect SNAPSHOT", Short: "Inspect a ready snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := snapshot.NewStore(cfg.Runtime.RootDir).Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeSnapshotTable(cmd.OutOrStdout(), []*snapshot.Record{rec})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newSnapshotRMCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "rm SNAPSHOT", Aliases: []string{"remove"}, Short: "Remove an unused snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := snapshot.NewStore(cfg.Runtime.RootDir).Remove(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
}

func writeSnapshotTable(w io.Writer, records []*snapshot.Record) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSIZE\tCREATED"); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", rec.ID, rec.Name, rec.State, rec.SizeBytes, rec.CreatedAt.Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
