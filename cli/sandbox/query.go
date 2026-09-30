package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// NewInspectCommand builds the read-only detailed sandbox query. Inspect always
// writes JSON so its complete output remains stable for people and scripts.
func NewInspectCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{
		Use:   "inspect SANDBOX",
		Short: "show detailed sandbox information as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "inspect sandbox", args[0], "close metadata", "retry the query", false))
			}()
			statuses, err := service.Status(command.Context(), args[0])
			if err != nil {
				return err
			}
			devices, err := service.AttachedDevices(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeStatusDetailJSON(command.OutOrStdout(), statuses[0], devices)
		},
	}
	return command
}

// NewLogsCommand builds the persistent VMM log reader. Follow mode writes only
// log bytes to stdout, leaving cancellation and diagnostics to the CLI shell.
func NewLogsCommand(configuration configProvider) *cobra.Command {
	var follow bool
	var tail int
	command := &cobra.Command{
		Use:   "logs [flags] SANDBOX",
		Short: "show sandbox VMM logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			reference := args[0]
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "read sandbox logs", reference, "close metadata", "retry the log stream", false))
			}()
			return service.Logs(command.Context(), reference, core.SandboxLogOptions{Tail: tail, Follow: follow}, command.OutOrStdout())
		},
	}
	command.Flags().BoolVarP(&follow, "follow", "f", false, "follow appended log output")
	command.Flags().IntVar(&tail, "tail", 0, "show only the last N lines (0 = all)")
	return command
}

// NewListCommand builds the top-level Docker-style sandbox process listing.
func NewListCommand(configuration configProvider) *cobra.Command {
	var includeAll, asJSON, quiet bool
	command := &cobra.Command{
		Use:   "ps",
		Short: "list sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			if asJSON && quiet {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("--json and --quiet cannot be used together"))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "list sandboxes", "", "close metadata", "retry the query", false))
			}()
			statuses, err := service.Status(command.Context())
			if err != nil {
				return err
			}
			records := make([]types.Sandbox, 0, len(statuses))
			visible := make([]core.SandboxStatus, 0, len(statuses))
			for _, status := range statuses {
				projected := projectStatus(status)
				if !includeAll && projected.State != string(types.SandboxStateStarting) && projected.State != string(types.SandboxStateRunning) && projected.State != string(types.SandboxStateStopping) {
					continue
				}
				status.Sandbox.State = types.SandboxState(projected.State)
				records = append(records, status.Sandbox)
				visible = append(visible, status)
			}
			switch {
			case asJSON:
				return writeStatusJSON(command.OutOrStdout(), visible)
			case quiet:
				return writeSandboxIDs(command.OutOrStdout(), records)
			default:
				return writeSandboxTable(command.OutOrStdout(), records)
			}
		},
	}
	command.Flags().BoolVarP(&includeAll, "all", "a", false, "show all sandboxes, including inactive states")
	command.Flags().BoolVar(&asJSON, "json", false, "print sandboxes as indented JSON")
	command.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only full sandbox IDs")
	return command
}
