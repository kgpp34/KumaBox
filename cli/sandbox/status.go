package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/types"
)

type statusOutput struct {
	sandboxOutput
	Runtime         string                 `json:"runtime_state,omitempty"`
	PID             int                    `json:"pid,omitempty"`
	Stale           bool                   `json:"stale,omitempty"`
	AttachedDevices *types.AttachedDevices `json:"attached_devices,omitempty"`
}

// NewStatusCommand builds a read-only view of durable and live sandbox state.
func NewStatusCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "status [SANDBOX...]",
		Short: "show sandbox records alongside their live VMM state",
		RunE: func(command *cobra.Command, references []string) (returnErr error) {
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			statuses, err := service.Status(command.Context(), references...)
			if err != nil {
				return err
			}
			if asJSON {
				return writeStatusJSON(command.OutOrStdout(), statuses)
			}
			return writeStatusTable(command.OutOrStdout(), statuses)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print status as indented JSON")
	return command
}

func writeStatusJSON(writer io.Writer, statuses []core.SandboxStatus) error {
	result := make([]statusOutput, 0, len(statuses))
	for _, status := range statuses {
		result = append(result, projectStatus(status))
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func writeStatusDetailJSON(writer io.Writer, status core.SandboxStatus, devices types.AttachedDevices) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	output := projectStatus(status)
	if len(devices.Disks) > 0 || len(devices.FS) > 0 || len(devices.Devices) > 0 {
		output.AttachedDevices = &devices
	}
	return encoder.Encode(output)
}

func writeStatusTable(writer io.Writer, statuses []core.SandboxStatus) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "SANDBOX ID\tNAME\tSTATE\tRUNTIME\tPID"); err != nil {
		return err
	}
	for _, status := range statuses {
		item := projectStatus(status)
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\n", item.ID, item.Name, item.State, item.Runtime, item.PID); err != nil {
			return err
		}
	}
	return table.Flush()
}

func projectStatus(status core.SandboxStatus) statusOutput {
	output := statusOutput{sandboxOutput: sandboxResult(status.Sandbox), Runtime: string(status.Runtime), PID: status.PID, Stale: status.Stale}
	if status.Stale {
		output.State = string(types.SandboxStateStopped)
	}
	return output
}
