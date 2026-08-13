package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	kbgc "github.com/kumabox/kumabox/internal/gc"
)

func newGCCommand(opts *rootOptions) *cobra.Command {
	var dryRun bool
	var repair bool
	var jsonOutput bool
	var snapshotKeep int
	var snapshotMaxAge time.Duration
	var snapshotMaxBytes string

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Inspect garbage-collection candidates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun && !repair {
				return fmt.Errorf("choose --dry-run or --repair")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			options := kbgc.Options{}
			policyEnabled := cmd.Flags().Changed("snapshot-keep") ||
				cmd.Flags().Changed("snapshot-max-age") ||
				cmd.Flags().Changed("snapshot-max-bytes")
			if policyEnabled {
				if snapshotKeep < 0 {
					return fmt.Errorf("--snapshot-keep must not be negative")
				}
				if snapshotMaxAge < 0 {
					return fmt.Errorf("--snapshot-max-age must not be negative")
				}
				var maxBytes int64
				if strings.TrimSpace(snapshotMaxBytes) != "" {
					maxBytes, err = parsePositiveByteSize("--snapshot-max-bytes", snapshotMaxBytes)
					if err != nil {
						return err
					}
				}
				options.SnapshotPolicy = &kbgc.SnapshotPolicy{
					KeepLast: snapshotKeep, KeepLastSet: cmd.Flags().Changed("snapshot-keep"),
					MaxAge: snapshotMaxAge, MaxBytes: maxBytes,
				}
			}
			var report *kbgc.Report
			if repair {
				report, err = kbgc.RepairWithOptions(cmd.Context(), cfg, options)
			} else {
				report, err = kbgc.DryRunContext(cmd.Context(), cfg, options)
			}
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
	cmd.Flags().BoolVar(&repair, "repair", false, "remove safe orphan resources and retry stale network cleanup")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	cmd.Flags().IntVar(&snapshotKeep, "snapshot-keep", 0, "keep at least this many newest snapshots per source VM")
	cmd.Flags().DurationVar(&snapshotMaxAge, "snapshot-max-age", 0, "evict snapshots not accessed within this duration")
	cmd.Flags().StringVar(&snapshotMaxBytes, "snapshot-max-bytes", "", "evict least-recently-used snapshots above this total size")
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
	if report.SnapshotPolicy != nil {
		for _, candidate := range report.SnapshotPolicy.Candidates {
			if _, err := fmt.Fprintf(tw, "snapshot-policy\t%s\t%s\t%s\n", candidate.Reason, candidate.Name, "eligible snapshot policy candidate"); err != nil {
				return err
			}
		}
		for _, candidate := range report.SnapshotPolicy.Blocked {
			if _, err := fmt.Fprintf(tw, "snapshot-policy\tblocked\t%s\t%s\n", candidate.Name, candidate.Reason); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}
