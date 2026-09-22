package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewRestoreCommand builds the top-level native snapshot restore command.
func NewRestoreCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "restore SANDBOX SNAPSHOT",
		Short: "restore a sandbox to a saved snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := startRestoreProgress(command, args[0])
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSnapshots(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "restore sandbox", args[0], "close metadata", "inspect the sandbox before retrying", committed))
			}()
			record, err := service.Restore(command.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "restore sandbox", args[0], "output", "sandbox is running; inspect it before retrying", true)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the restored sandbox as indented JSON")
	return command
}
