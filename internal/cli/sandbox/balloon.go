package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/client"
	"github.com/kumabox/kumabox/internal/types"
)

// NewBalloonCommand sets a running sandbox's asynchronous memory reclaim target.
func NewBalloonCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "balloon SANDBOX SIZE",
		Short: "set reclaimed memory in bytes or percent (for example 256MiB, 25%, or 0)",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			target, err := types.ParseBalloonTarget(args[1])
			if err != nil {
				return invalidFlag("balloon", err)
			}
			service, err := client.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			status, err := service.ResizeBalloon(command.Context(), args[0], target)
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Balloon target set to %d bytes. Guest reclaim continues asynchronously.\n", status.Target)
			return err
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the target and current reclaim as indented JSON")
	return command
}
