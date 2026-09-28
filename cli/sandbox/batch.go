package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// runSandboxBatch shares one metadata session while processing independent
// lifecycle targets. A failed reference does not prevent later references
// from completing; the caller receives the joined failures and all successes.
func runSandboxBatch(
	command *cobra.Command, configuration configProvider, references []string, operation string,
	asJSON, removed bool, action func(context.Context, *core.SandboxService, string) (types.Sandbox, error),
) (returnErr error) {
	service, err := core.OpenSandbox(command.Context(), configuration(), nil)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), operation, "", "close metadata", "inspect completed targets before retrying", false))
	}()
	succeeded := make([]types.Sandbox, 0, len(references))
	var failures []error
	for _, reference := range references {
		if err := command.Context().Err(); err != nil {
			failures = append(failures, err)
			break
		}
		record, err := action(command.Context(), service, reference)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", reference, err))
			continue
		}
		succeeded = append(succeeded, record)
	}
	var outputErr error
	if asJSON {
		if removed {
			outputErr = writeRemoveListJSON(command.OutOrStdout(), succeeded)
		} else {
			outputErr = writeSandboxListJSON(command.OutOrStdout(), succeeded)
		}
	} else {
		outputErr = writeSandboxIDs(command.OutOrStdout(), succeeded)
	}
	return errors.Join(errors.Join(failures...), errdefs.Context(outputErr, operation, "", "output", "inspect completed targets before retrying", len(succeeded) > 0))
}

func writeRemoveListJSON(writer io.Writer, records []types.Sandbox) error {
	results := make([]removeOutput, 0, len(records))
	for _, record := range records {
		results = append(results, removeOutput{ID: record.ID.String(), Name: record.Config.Name})
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}
