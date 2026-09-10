// Package doctor implements "kumabox doctor".
package doctor

import (
	"github.com/spf13/cobra"
)

// NewCommand builds the doctor command.
func NewCommand() *cobra.Command {
	var options Options

	command := &cobra.Command{
		Use:   "doctor",
		Short: "check whether this machine can run KumaBox",
		Long: "doctor inspects this machine and reports, check by check, whether it is\n" +
			"ready for the phase being worked on.\n\n" +
			"A check that belongs to a later phase is reported as not-required and never\n" +
			"fails the command. --fix only creates or repairs KumaBox-owned directories;\n" +
			"installing packages, sysctl and firewall rules belong to hack/host-install.sh.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			rootFlag, err := command.Flags().GetString("root")
			if err != nil {
				return err
			}
			options.Root = rootFlag

			handler := Handler{}
			report, err := handler.Doctor(command.Context(), options)
			if err != nil {
				return err
			}
			if err := handler.Render(command.OutOrStdout(), report, options.JSON); err != nil {
				return err
			}
			return handler.NotReady(report)
		},
	}
	command.Flags().BoolVar(&options.JSON, "json", false, "print the report as JSON")
	command.Flags().BoolVar(&options.Fix, "fix", false, "create or repair KumaBox-owned directories")
	return command
}
