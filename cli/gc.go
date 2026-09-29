package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
)

// newGCCommand exposes the same lock-safe collector used by daemon sweeps.
func newGCCommand(configuration func() config.Config) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "gc",
		Short: "repair interrupted operations and reclaim orphaned resources",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			service, err := core.OpenSnapshots(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			report, collectErr := service.Collect(command.Context())
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
	return command
}
