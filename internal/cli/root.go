package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/doctor"
	"github.com/kumabox/kumabox/internal/version"
	"github.com/kumabox/kumabox/internal/vmstore"
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
	cmd.AddCommand(newCreateCommand(opts))
	cmd.AddCommand(newInspectCommand(opts))
	cmd.AddCommand(newPSCommand(opts))
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

func newCreateCommand(opts *rootOptions) *cobra.Command {
	var name string
	var rootDisk string
	var kernel string
	var initrd string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a VM record",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			store := vmstore.New(cfg.Runtime.RootDir)
			rec, err := store.Create(vmstore.CreateRequest{
				Name:     name,
				RootDisk: rootDisk,
				Kernel:   kernel,
				Initrd:   initrd,
				RunDir:   cfg.Runtime.RunDir,
				LogDir:   cfg.Runtime.LogDir,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "VM name")
	cmd.Flags().StringVar(&rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&initrd, "initrd", "", "initrd image path")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("root-disk")
	_ = cmd.MarkFlagRequired("kernel")
	_ = cmd.MarkFlagRequired("initrd")
	return cmd
}

func newInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "inspect VM",
		Short: "Inspect a VM record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := vmstore.New(cfg.Runtime.RootDir).Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", rec.ID, rec.Name, rec.State)
			return err
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newPSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List VM records",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			records, err := vmstore.New(cfg.Runtime.RootDir).List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			for _, rec := range records {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", rec.ID, rec.Name, rec.State)
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
