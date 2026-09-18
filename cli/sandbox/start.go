package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewStartCommand builds the top-level sandbox start command.
func NewStartCommand(configuration configProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:   "start SANDBOX",
		Short: "start a created or stopped sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			reference := args[0]
			progress, err := startStartProgress(command, reference)
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
				returnErr = errors.Join(returnErr, errdefs.Context(closeErr, "start sandbox", reference, "close metadata", "inspect the sandbox before retrying", committed))
			}()
			record, err := service.Start(command.Context(), reference)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "start sandbox", reference, "output", "sandbox is running; inspect it before retrying", true)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the running sandbox as indented JSON")
	return command
}
