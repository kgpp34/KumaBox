package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

type lifecycleBatchOutput struct {
	Succeeded []string                 `json:"succeeded"`
	Failed    []kbruntime.BatchFailure `json:"failed,omitempty"`
}

func addBatchConcurrencyFlag(cmd *cobra.Command, concurrency *int) {
	cmd.Flags().IntVar(concurrency, "concurrency", 0, "maximum concurrent VM operations; 0 uses host CPU count")
}

func addResourceBatchConcurrencyFlag(cmd *cobra.Command, concurrency *int) {
	cmd.Flags().IntVar(concurrency, "concurrency", 0, "maximum concurrent operations; 0 uses host CPU count")
}

func validateBatchConcurrency(concurrency int) error {
	if concurrency < 0 {
		return errors.New("concurrency must be greater than or equal to zero")
	}
	return nil
}

func lifecycleBatchOptions(concurrency int) (kbruntime.BatchOptions, error) {
	if err := validateBatchConcurrency(concurrency); err != nil {
		return kbruntime.BatchOptions{}, err
	}
	return kbruntime.BatchOptions{Concurrency: concurrency}, nil
}

func writeLifecycleBatchResult(
	cmd *cobra.Command,
	refs []string,
	operation string,
	result kbruntime.BatchResult,
) error {
	if len(refs) == 1 {
		if err := result.Err(); err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result.Succeeded[0])
	}
	output := lifecycleBatchOutput{
		Succeeded: make([]string, 0, len(result.Succeeded)),
		Failed:    result.Failed,
	}
	for _, record := range result.Succeeded {
		output.Succeeded = append(output.Succeeded, record.ID)
	}
	if err := writeJSON(cmd.OutOrStdout(), output); err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
