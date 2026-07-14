package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/snapshot"
)

func newSnapshotCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "snapshot", Short: "Manage stopped VM snapshots"}
	cmd.AddCommand(newSnapshotLSCommand(opts))
	cmd.AddCommand(newSnapshotInspectCommand(opts))
	cmd.AddCommand(newSnapshotRMCommand(opts))
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
