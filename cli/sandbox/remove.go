package sandbox

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewRemoveCommand builds the top-level sandbox removal command.
func NewRemoveCommand(configuration configProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:   "rm SANDBOX...",
		Short: "remove a sandbox and its persistent resources",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if len(args) > 1 {
				return runSandboxBatch(command, configuration, args, "remove sandbox", asJSON, true,
					func(ctx context.Context, service *core.SandboxService, reference string) (types.Sandbox, error) {
						return service.Remove(ctx, reference)
					})
			}
			reference := args[0]
			progress, err := startRemoveProgress(command, reference)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSandbox(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				closeErr := service.Close()
				returnErr = errors.Join(returnErr, errdefs.WithContext(closeErr, errdefs.ContextInfo{
					Operation: "remove sandbox",
					Entity:    reference,
					Phase:     "close metadata",
					Action:    "inspect the sandbox before retrying",
					Committed: committed,
				}))
			}()
			removed, err := service.Remove(command.Context(), reference)
			if err != nil {
				return err
			}
			committed = true
			if err := writeRemoveResult(progress.Output(command.OutOrStdout()), removed, asJSON); err != nil {
				return errdefs.WithContext(err, errdefs.ContextInfo{
					Operation: "remove sandbox",
					Entity:    reference,
					Phase:     "output",
					Action:    "sandbox was deleted; do not retry",
					Committed: true,
				})
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the removed sandbox as indented JSON")
	return command
}
