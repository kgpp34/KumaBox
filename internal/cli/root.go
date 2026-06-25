package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/doctor"
	"github.com/kumabox/kumabox/internal/version"
)

type rootOptions struct {
	configPath string
	rootDir    string
	runDir     string
	logDir     string
}

func NewRootCommand() *cobra.Command {
	opts := &rootOptions{}

	cmd := &cobra.Command{
		Use:           "kumabox",
		Short:         "KumaBox microVM sandbox runtime",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&opts.configPath, "config", "", "config file path")
	cmd.PersistentFlags().StringVar(&opts.rootDir, "root-dir", "", "persistent state directory")
	cmd.PersistentFlags().StringVar(&opts.runDir, "run-dir", "", "runtime directory for pid and sockets")
	cmd.PersistentFlags().StringVar(&opts.logDir, "log-dir", "", "log directory")

	cmd.AddCommand(newVersionCommand())
	cmd.AddCommand(newDoctorCommand(opts))
	return cmd
}

func newVersionCommand() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := version.Info()
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "kumabox %s (%s, built %s)\n", info.Version, info.Commit, info.BuildTime)
			return err
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

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

func loadConfig(opts *rootOptions) (config.Config, error) {
	overrides := config.Overrides{
		RootDir: opts.rootDir,
		RunDir:  opts.runDir,
		LogDir:  opts.logDir,
	}
	return config.Load(opts.configPath, overrides)
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeDoctorText(w io.Writer, report doctor.Report) {
	fmt.Fprintf(w, "doctor: %s\n", report.Status)
	for _, check := range report.Checks {
		if check.Code != "" {
			fmt.Fprintf(w, "%s: %s (%s): %s\n", check.Status, check.Name, check.Code, check.Message)
			continue
		}
		fmt.Fprintf(w, "%s: %s: %s\n", check.Status, check.Name, check.Message)
	}
}
