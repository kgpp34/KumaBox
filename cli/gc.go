package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// newGCCommand exposes the same lock-safe collector used by daemon sweeps.
func newGCCommand(configuration func() config.Config) *cobra.Command {
	var asJSON, evictSnapshots, dryRun bool
	var keepLast int
	var maxAge time.Duration
	var maxSizeText string
	command := &cobra.Command{
		Use:   "gc",
		Short: "repair interrupted operations and reclaim orphaned resources",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			for _, name := range []string{"snapshot-keep", "snapshot-age", "snapshot-size", "snapshot-dry-run"} {
				if command.Flags().Changed(name) && !evictSnapshots {
					return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--%s requires --snapshot", name))
				}
			}
			if keepLast < 0 || maxAge < 0 {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("snapshot keep and age limits must not be negative"))
			}
			var maxSize int64
			if maxSizeText != "" {
				var err error
				maxSize, err = types.ParseByteSize(maxSizeText)
				if err != nil {
					return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--snapshot-size: %w", err))
				}
			}
			service, err := core.OpenMaintenance(command.Context(), configuration())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			report, collectErr := service.CollectWithPolicy(command.Context(), core.SnapshotEvictionPolicy{
				Enabled: evictSnapshots, DryRun: dryRun, KeepLast: keepLast, MaxAge: maxAge, MaxSize: maxSize,
			})
			if asJSON {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return errors.Join(collectErr, encoder.Encode(report))
			}
			table := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(table, "KIND\tID\tACTION"); err != nil {
				return errors.Join(collectErr, err)
			}
			for _, action := range report.Actions {
				if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", action.Kind, action.ID, action.Action); err != nil {
					return errors.Join(collectErr, err)
				}
			}
			if _, err := fmt.Fprintf(table, "completed=%d skipped=%d\n", len(report.Actions), report.Skipped); err != nil {
				return errors.Join(collectErr, err)
			}
			return errors.Join(collectErr, table.Flush())
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the collection report as indented JSON")
	command.Flags().BoolVar(&evictSnapshots, "snapshot", false, "evict ready snapshots by LRU (without limits, evict all)")
	command.Flags().IntVar(&keepLast, "snapshot-keep", 0, "keep this many most recently used snapshots")
	command.Flags().DurationVar(&maxAge, "snapshot-age", 0, "evict snapshots not used within this duration")
	command.Flags().StringVar(&maxSizeText, "snapshot-size", "", "evict oldest snapshots until total size fits this limit")
	command.Flags().BoolVar(&dryRun, "snapshot-dry-run", false, "preview snapshot eviction; orphan cleanup still runs")
	return command
}
