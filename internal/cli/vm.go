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
	var concurrency int

	cmd := &cobra.Command{
		Use:   "start VM [VM...]",
		Short: "Start one or more VMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			batchOpts, err := lifecycleBatchOptions(concurrency)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			result := rt.StartVMsContext(cmd.Context(), args, batchOpts)
			return writeLifecycleBatchResult(cmd, args, "start", result)
		},
	}
	addBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}

func newStopCommand(opts *rootOptions) *cobra.Command {
	var timeout time.Duration
	var force bool
	var concurrency int

	cmd := &cobra.Command{
		Use:   "stop VM [VM...]",
		Short: "Stop one or more VMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			batchOpts, err := lifecycleBatchOptions(concurrency)
			if err != nil {
				return err
			}
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
			result := rt.StopVMsContext(cmd.Context(), args, backend.StopOptions{
				Timeout: timeout,
				Force:   force,
			}, batchOpts)
			return writeLifecycleBatchResult(cmd, args, "stop", result)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 0, "graceful shutdown timeout")
	cmd.Flags().BoolVar(&force, "force", false, "skip API shutdown and terminate the VMM")
	addBatchConcurrencyFlag(cmd, &concurrency)
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
	var follow bool
	var interval time.Duration

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
			selectedSource := source
			if follow && !cmd.Flags().Changed("source") {
				selectedSource = kbruntime.LogSourceVMM
			}
			logOpts := kbruntime.LogOptions{
				Tail:   tail,
				Source: selectedSource,
			}
			if follow {
				multiple := len(kbruntime.LogFileNames(selectedSource)) > 1
				return rt.FollowLogsVM(cmd.Context(), args[0], logOpts, interval, func(chunk kbruntime.VMLogChunk) error {
					if jsonOutput {
						return writeJSONLine(cmd.OutOrStdout(), chunk)
					}
					return writeVMLogChunk(cmd.OutOrStdout(), chunk, multiple)
				})
			}
			logs, err := rt.LogsVM(args[0], logOpts)
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
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream appended log content until interrupted")
	cmd.Flags().DurationVar(&interval, "interval", 200*time.Millisecond, "poll interval used while following")
	return cmd
}

func newDeleteCommand(opts *rootOptions) *cobra.Command {
	var force bool
	var concurrency int

	cmd := &cobra.Command{
		Use:   "delete VM [VM...]",
		Short: "Delete one or more VMs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			batchOpts, err := lifecycleBatchOptions(concurrency)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			result := rt.DeleteVMsContext(cmd.Context(), args, force, batchOpts)
			return writeLifecycleBatchResult(cmd, args, "delete", result)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "stop running VM before deleting it")
	addBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}

func newPSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool
	var watch bool
	var events bool
	var interval time.Duration
	var eventHeader bool

	cmd := &cobra.Command{
		Use:   "ps [VM...]",
		Short: "List or watch VM records",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			if watch || events {
				return rt.WatchVMs(cmd.Context(), args, interval, func(update kbruntime.VMStatusUpdate) error {
					if events {
						if jsonOutput {
							for _, event := range update.Events {
								if err := writeJSONLine(cmd.OutOrStdout(), event); err != nil {
									return err
								}
							}
							return nil
						}
						err := writeVMEventTable(cmd.OutOrStdout(), update.Events, !eventHeader)
						eventHeader = true
						return err
					}
					if jsonOutput {
						return writeJSONLine(cmd.OutOrStdout(), update.Records)
					}
					return writeVMTable(cmd.OutOrStdout(), update.Records)
				})
			}
			records, err := rt.ListVMs()
			if err != nil {
				return err
			}
			records = filterVMRecords(records, args)
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeVMTable(cmd.OutOrStdout(), records)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "watch and redraw when VM status changes")
	cmd.Flags().BoolVar(&events, "events", false, "stream ADDED, MODIFIED, and DELETED events")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval used while watching")
	return cmd
}

func filterVMRecords(records []*vmstore.VMRecord, refs []string) []*vmstore.VMRecord {
	if len(refs) == 0 {
		return records
	}
	selected := make([]*vmstore.VMRecord, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		for _, record := range records {
			if record.ID != ref && record.Name != ref {
				continue
			}
			if _, ok := seen[record.ID]; !ok {
				selected = append(selected, record)
				seen[record.ID] = struct{}{}
			}
			break
		}
	}
	return selected
}
