package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewRunCommand builds the top-level create-and-start command.
func NewRunCommand(configuration configProvider) *cobra.Command {
	options := defaultCreateOptions()
	asJSON := false
	command := &cobra.Command{
		Use:   "run IMAGE",
		Short: "create and start a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			request, err := options.request(args[0])
			if err != nil {
				return err
			}
			progress, err := startRunProgress(command, options.name)
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
				returnErr = errors.Join(returnErr, errdefs.Context(
					closeErr, "run sandbox", options.name, "close metadata",
					"inspect the sandbox before retrying", committed,
				))
			}()

			record, err := service.Run(command.Context(), request)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "run sandbox", options.name, "output", "sandbox is running; inspect it before retrying", true)
			}
			return nil
		},
	}
	options.addFlags(command)
	command.Flags().BoolVar(&asJSON, "json", false, "print the running sandbox as indented JSON")
	return command
}
