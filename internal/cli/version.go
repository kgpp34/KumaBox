package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/version"
)

func newVersionCommand() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := version.Info()
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "kumabox %s (%s, built %s)\n", info.Version, info.Commit, info.BuildTime)
			return err
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
