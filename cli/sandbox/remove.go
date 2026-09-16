package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewRemoveCommand builds the top-level sandbox removal command.
func NewRemoveCommand(roots rootsProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:   "rm SANDBOX",
		Short: "remove a sandbox and its persistent resources",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			reference := args[0]
			progress, err := startRemoveProgress(command, reference)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSandbox(command.Context(), roots(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				closeErr := service.Close()
				returnErr = errors.Join(returnErr, errdefs.Context(closeErr, "remove sandbox", reference, "close metadata", "inspect the sandbox before retrying", committed))
			}()
			removed, err := service.Remove(command.Context(), reference)
			if err != nil {
				return err
			}
			committed = true
			if err := writeRemoveResult(progress.Output(command.OutOrStdout()), removed, asJSON); err != nil {
				return errdefs.Context(err, "remove sandbox", reference, "output", "sandbox was deleted; do not retry", true)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the removed sandbox as indented JSON")
	return command
}
