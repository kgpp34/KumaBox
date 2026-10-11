package sandbox

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewStopCommand builds the top-level sandbox stop command.
func NewStopCommand(configuration configProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:   "stop SANDBOX...",
		Short: "stop a running or interrupted sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if len(args) > 1 {
				return runSandboxBatch(command, configuration, args, "stop sandbox", asJSON, false,
					func(ctx context.Context, service *core.SandboxService, reference string) (types.Sandbox, error) {
						return service.Stop(ctx, reference)
					})
			}
			reference := args[0]
			progress, err := startStopProgress(command, reference)
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
					Operation: "stop sandbox",
					Entity:    reference,
					Phase:     "close metadata",
					Action:    "inspect the sandbox before retrying",
					Committed: committed,
				}))
			}()
			record, err := service.Stop(command.Context(), reference)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.WithContext(err, errdefs.ContextInfo{
					Operation: "stop sandbox",
					Entity:    reference,
					Phase:     "output",
					Action:    "sandbox is stopped; inspect it before retrying",
					Committed: true,
				})
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the stopped sandbox as indented JSON")
	return command
}
