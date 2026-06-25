package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/doctor"
)

func newDoctorCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host requirements",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}

			report := doctor.Run(cfg)
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				writeDoctorText(cmd.OutOrStdout(), report)
			}

			if report.Status != doctor.StatusPass {
				return fmt.Errorf("doctor checks failed")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
