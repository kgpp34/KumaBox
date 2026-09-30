package sandbox

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewNetCommand resizes the NIC count of one running sandbox.
func NewNetCommand(configuration configProvider) *cobra.Command {
	var nics int
	var asJSON bool
	command := &cobra.Command{
		Use:   "net SANDBOX --nics N",
		Short: "resize a running sandbox's network interfaces",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if !command.Flags().Changed("nics") {
				return invalidFlag("nics", errors.New("is required"))
			}
			if nics < 0 || nics > types.MaxSandboxNICs {
				return invalidFlag("nics", fmt.Errorf("must be between 0 and %d", types.MaxSandboxNICs))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.WithContext(service.Close(), errdefs.ContextInfo{
					Operation: "resize sandbox network",
					Entity:    args[0],
					Phase:     "close metadata",
					Action:    "inspect the sandbox",
				}))
			}()
			record, err := service.NetResize(command.Context(), args[0], nics)
			if err != nil {
				return err
			}
			return writeSandboxResult(command.OutOrStdout(), record, asJSON)
		},
	}
	command.Flags().IntVar(&nics, "nics", 0, "target number of network interfaces, including zero")
	command.Flags().BoolVar(&asJSON, "json", false, "print the updated sandbox as indented JSON")
	return command
}
