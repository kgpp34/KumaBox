package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewProcessCommand manages guest background commands. VM logs/ps remain
// separate: these subcommands operate only on the guest agent's process table.
func NewProcessCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "process", Short: "manage background guest commands", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	command.AddCommand(newProcessList(configuration), newProcessKill(configuration), newProcessOutput(configuration, "logs"), newProcessOutput(configuration, "attach"))
	return command
}

func newProcessList(configuration configProvider) *cobra.Command {
	var all, asJSON bool
	command := &cobra.Command{
		Use: "ls SANDBOX", Aliases: []string{"list"}, Short: "list running agent-owned commands", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return withGuestService(command, configuration, func(service *core.SandboxService) error {
				records, err := service.Processes(command.Context(), args[0])
				if err != nil {
					return err
				}
				visible := make([]types.ProcessInfo, 0, len(records))
				for _, record := range records {
					if all || record.Running {
						visible = append(visible, record)
					}
				}
				if asJSON {
					return writeGuestJSON(command.OutOrStdout(), visible)
				}
				table := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
				if _, err := fmt.Fprintln(table, "PROCESS ID\tPID\tSTATE\tEXIT\tCOMMAND"); err != nil {
					return err
				}
				for _, record := range visible {
					state, exit := "running", "-"
					if !record.Running {
						state = "exited"
					}
					if record.ExitCode != nil {
						exit = fmt.Sprint(*record.ExitCode)
					}
					if _, err := fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%q\n", record.ID, record.PID, state, exit, record.Args); err != nil {
						return err
					}
				}
				return table.Flush()
			})
		},
	}
	command.Flags().BoolVarP(&all, "all", "a", false, "include retained exited commands")
	command.Flags().BoolVar(&asJSON, "json", false, "print process records as indented JSON")
	return command
}

func newProcessKill(configuration configProvider) *cobra.Command {
	signal := string(types.ProcessSignalTERM)
	command := &cobra.Command{
		Use: "kill SANDBOX PROCESS_ID", Short: "signal a background command (TERM by default)", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			selected := types.ProcessSignal(strings.TrimPrefix(strings.ToUpper(signal), "SIG"))
			if err := types.ValidateProcessID(args[1]); err != nil {
				return invalidFlag("process-id", err)
			}
			if err := selected.Validate(); err != nil {
				return invalidFlag("signal", err)
			}
			return withGuestService(command, configuration, func(service *core.SandboxService) error {
				return service.SignalProcess(command.Context(), args[0], args[1], selected)
			})
		},
	}
	command.Flags().StringVarP(&signal, "signal", "s", signal, "TERM, KILL, INT or HUP")
	return command
}

func newProcessOutput(configuration configProvider, verb string) *cobra.Command {
	var after uint64
	command := &cobra.Command{
		Use: verb + " SANDBOX PROCESS_ID", Short: "replay background command output", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := types.ValidateProcessID(args[1]); err != nil {
				return invalidFlag("process-id", err)
			}
			return withGuestService(command, configuration, func(service *core.SandboxService) error {
				options := types.ProcessOutputOptions{After: after}
				var result types.ProcessOutputResult
				var err error
				if verb == "attach" {
					result, err = service.AttachProcess(command.Context(), args[0], args[1], options, command.OutOrStdout(), command.ErrOrStderr())
				} else {
					result, err = service.ProcessLogs(command.Context(), args[0], args[1], options, command.OutOrStdout(), command.ErrOrStderr())
				}
				if result.Truncated {
					_, warnErr := fmt.Fprintln(command.ErrOrStderr(), "kumabox: older process output was overwritten by the bounded log buffer")
					err = errors.Join(err, warnErr)
				}
				if err != nil {
					return err
				}
				if verb == "attach" && result.ExitCode != nil && *result.ExitCode != 0 {
					return &commandExitError{code: *result.ExitCode}
				}
				return nil
			})
		},
	}
	if verb == "attach" {
		command.Short = "replay and follow command output until exit; Ctrl-C only detaches"
	}
	command.Flags().Uint64Var(&after, "after", 0, "replay strictly after this output frame cursor")
	return command
}

func withGuestService(command *cobra.Command, configuration configProvider, run func(*core.SandboxService) error) (returnErr error) {
	service, err := core.OpenSandbox(command.Context(), configuration(), nil)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.WithContext(service.Close(), errdefs.ContextInfo{Operation: "access guest agent", Phase: "close metadata", Action: "inspect the guest before retrying"}))
	}()
	return run(service)
}

func writeGuestJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
