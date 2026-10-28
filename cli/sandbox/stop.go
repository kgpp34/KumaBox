package sandbox

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/client"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewStopCommand builds the top-level sandbox stop command.
func NewStopCommand(configuration configProvider) *cobra.Command {
	asJSON := false
	force := false
	timeoutSeconds := int64(0)
	command := &cobra.Command{
		Use:   "stop SANDBOX...",
		Short: "shut down a sandbox, escalating to force termination on timeout",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if timeoutSeconds < 0 || timeoutSeconds > math.MaxInt64/int64(time.Second) {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("--timeout must be non-negative seconds within the supported duration range"))
			}
			options := types.StopOptions{Force: force, Timeout: time.Duration(timeoutSeconds) * time.Second}
			if len(args) > 1 {
				return runSandboxBatch(command, configuration, args, "stop sandbox", asJSON, false,
					func(ctx context.Context, service *client.SandboxService, reference string) (types.Sandbox, error) {
						return service.StopWithOptions(ctx, reference, options)
					})
			}
			reference := args[0]
			progress, err := startStopProgress(command, reference)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := client.OpenSandbox(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				closeErr := service.Close()
				returnErr = errors.Join(returnErr, errdefs.WithContext(closeErr, errdefs.ContextInfo{
					Operation: "stop sandbox",
					Entity:    reference,
					Phase:     "close client",
					Action:    "inspect the sandbox before retrying",
					Committed: committed,
				}))
			}()
			record, err := service.StopWithOptions(command.Context(), reference, options)
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
	command.Flags().BoolVarP(&force, "force", "f", false, "skip guest shutdown and begin VMM termination immediately")
	command.Flags().Int64Var(&timeoutSeconds, "timeout", 0, "guest shutdown timeout in seconds (0 uses sandbox.stop_timeout; ignored with --force)")
	return command
}
