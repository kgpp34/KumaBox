package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewListCommand builds the top-level Docker-style sandbox process listing.
func NewListCommand(roots rootsProvider) *cobra.Command {
	var includeAll, asJSON, quiet bool
	command := &cobra.Command{
		Use:   "ps",
		Short: "list sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			if asJSON && quiet {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("--json and --quiet cannot be used together"))
			}
			service, err := core.OpenSandbox(command.Context(), roots(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "list sandboxes", "", "close metadata", "retry the query", false))
			}()
			records, err := service.List(command.Context(), includeAll)
			if err != nil {
				return err
			}
			switch {
			case asJSON:
				return writeSandboxListJSON(command.OutOrStdout(), records)
			case quiet:
				return writeSandboxIDs(command.OutOrStdout(), records)
			default:
				return writeSandboxTable(command.OutOrStdout(), records)
			}
		},
	}
	command.Flags().BoolVarP(&includeAll, "all", "a", false, "show all sandboxes, including inactive states")
	command.Flags().BoolVar(&asJSON, "json", false, "print sandboxes as indented JSON")
	command.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only full sandbox IDs")
	return command
}
