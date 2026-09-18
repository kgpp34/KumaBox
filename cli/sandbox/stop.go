package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewStopCommand builds the top-level sandbox stop command.
func NewStopCommand(configuration configProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:   "stop SANDBOX",
		Short: "stop a running or interrupted sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
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
				returnErr = errors.Join(returnErr, errdefs.Context(closeErr, "stop sandbox", reference, "close metadata", "inspect the sandbox before retrying", committed))
			}()
			record, err := service.Stop(command.Context(), reference)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "stop sandbox", reference, "output", "sandbox is stopped; inspect it before retrying", true)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the stopped sandbox as indented JSON")
	return command
}
