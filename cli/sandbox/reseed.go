package sandbox

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
)

// NewReseedCommand sends fresh host entropy to a running guest.
func NewReseedCommand(configuration configProvider) *cobra.Command {
	var machineID bool
	command := &cobra.Command{
		Use:   "reseed SANDBOX",
		Short: "refresh a running guest's random state",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			if err := service.Reseed(command.Context(), args[0], machineID); err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), args[0])
			return err
		},
	}
	command.Flags().BoolVar(&machineID, "machine-id", false, "also renew /etc/machine-id (for clones)")
	return command
}
