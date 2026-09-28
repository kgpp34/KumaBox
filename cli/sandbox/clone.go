package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewCloneCommand builds a running sandbox from a saved native snapshot.
func NewCloneCommand(configuration configProvider) *cobra.Command {
	var name string
	var asJSON bool
	command := &cobra.Command{
		Use:   "clone SNAPSHOT --name NAME",
		Short: "clone a snapshot into a new running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if name == "" {
				return invalidFlag("name", errors.New("is required"))
			}
			progress, err := startCloneProgress(command, name)
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
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "clone sandbox", name, "close metadata", "inspect the clone before retrying", committed))
			}()
			record, err := service.Clone(command.Context(), args[0], name)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "clone sandbox", name, "output", "clone is running; inspect it", true)
			}
			return nil
		},
	}
	command.Flags().StringVar(&name, "name", "", "required name for the new sandbox")
	command.Flags().BoolVar(&asJSON, "json", false, "print the cloned sandbox as indented JSON")
	return command
}
