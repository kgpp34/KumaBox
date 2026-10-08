package sandbox

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewNetCommand resizes NICs or applies the recorded network inside a guest.
func NewNetCommand(configuration configProvider) *cobra.Command {
	var nics int
	var configure bool
	var asJSON bool
	command := &cobra.Command{
		Use:   "net SANDBOX (--nics N | --configure)",
		Short: "resize NICs or configure a running sandbox's guest network",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if command.Flags().Changed("nics") == configure {
				return invalidFlag("nics", errors.New("specify exactly one of --nics or --configure"))
			}
			if !configure && (nics < 0 || nics > types.MaxSandboxNICs) {
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
			var record types.Sandbox
			if configure {
				record, err = service.NetConfigure(command.Context(), args[0])
			} else {
				record, err = service.NetResize(command.Context(), args[0], nics)
			}
			if err != nil {
				return err
			}
			return writeSandboxResult(command.OutOrStdout(), record, asJSON)
		},
	}
	command.Flags().IntVar(&nics, "nics", 0, "target number of network interfaces, including zero")
	command.Flags().BoolVar(&configure, "configure", false, "apply the recorded hostname and NIC settings inside the guest")
	command.Flags().BoolVar(&asJSON, "json", false, "print the updated sandbox as indented JSON")
	return command
}
