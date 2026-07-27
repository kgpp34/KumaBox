package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	defaultOCIStorageSize = "4G"
	defaultOCIMemorySize  = "512M"
)

func errInvalidLogSource(source string) error {
	return fmt.Errorf("invalid log source %q: expected console, stdout, stderr, vmm, or all", source)
}

func newCreateCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{}

	cmd := &cobra.Command{
		Use:   "create [IMAGE]",
		Short: "Create a VM record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			req, err := newCreateRequest(flags, args, cfg)
			if err != nil {
				return err
			}
			rec, err := rt.CreateVM(req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	addCreateVMFlags(cmd, &flags)
	return cmd
}

func newRunCommand(opts *rootOptions) *cobra.Command {
	flags := createVMFlags{}
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "run [IMAGE]",
		Short: "Create and start a VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}

			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			req, err := newCreateRequest(flags, args, cfg)
			if err != nil {
				return err
			}
			runContext := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				runContext, cancel = context.WithTimeout(runContext, timeout)
				defer cancel()
			}
			rec, err := rt.RunVMContext(runContext, req)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	addCreateVMFlags(cmd, &flags)
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "VM startup and guest readiness timeout")
	return cmd
}

func newStartCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start VM",
		Short: "Start a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.StartVMContext(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	return cmd
}

func newStopCommand(opts *rootOptions) *cobra.Command {
	var timeout time.Duration
	var force bool

	cmd := &cobra.Command{
		Use:   "stop VM",
		Short: "Stop a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if timeout <= 0 && cfg.Backend.CloudHypervisor.StopTimeoutMS > 0 {
				timeout = time.Duration(cfg.Backend.CloudHypervisor.StopTimeoutMS) * time.Millisecond
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.StopVMContext(cmd.Context(), args[0], backend.StopOptions{
				Timeout: timeout,
				Force:   force,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 0, "graceful shutdown timeout")
	cmd.Flags().BoolVar(&force, "force", false, "skip API shutdown and terminate the VMM")
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
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.InspectVM(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeVMTable(cmd.OutOrStdout(), []*vmstore.VMRecord{rec})
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newLogsCommand(opts *rootOptions) *cobra.Command {
	var tail int
	var source string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "logs VM",
		Short: "Show VM logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if !kbruntime.ValidLogSource(source) {
				return errInvalidLogSource(source)
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			logs, err := rt.LogsVM(args[0], kbruntime.LogOptions{
				Tail:   tail,
				Source: source,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), logs)
			}
			return writeVMLogs(cmd.OutOrStdout(), logs)
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 100, "number of recent lines to show, 0 for all")
	cmd.Flags().StringVar(&source, "source", kbruntime.LogSourceConsole, "log source: console, stdout, stderr, vmm, all")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newDeleteCommand(opts *rootOptions) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete VM",
		Short: "Delete a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.DeleteVMContext(cmd.Context(), args[0], force)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop running VM before deleting it")
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
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			records, err := rt.ListVMs()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeVMTable(cmd.OutOrStdout(), records)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
