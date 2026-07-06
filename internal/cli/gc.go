package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	kbgc "github.com/kumabox/kumabox/internal/gc"
)

func newGCCommand(opts *rootOptions) *cobra.Command {
	var dryRun bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Inspect garbage-collection candidates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				return fmt.Errorf("gc currently supports --dry-run only")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			report, err := kbgc.DryRun(cfg)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), report)
			}
			return writeGCReport(cmd.OutOrStdout(), report)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show candidates without deleting anything")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func writeGCReport(w io.Writer, report *kbgc.Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "COMPONENT\tTYPE\tPATH\tREASON"); err != nil {
		return err
	}
	for _, candidate := range report.Candidates {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", candidate.Component, candidate.Type, candidate.Path, candidate.Reason); err != nil {
			return err
		}
	}
	return tw.Flush()
}
