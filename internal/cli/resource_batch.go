package cli

import (
	"fmt"

	"github.com/kumabox/kumabox/internal/batch"
)

type resourceBatchFailure = batch.Failure

func writeResourceBatchResult[T any](cmdOutput func(any) error, refs []string, operation string, result batch.Result[T]) error {
	if len(refs) == 1 {
		if err := result.Err(); err != nil {
			return err
		}
		return cmdOutput(result.Succeeded[0])
	}
	if err := cmdOutput(struct {
		Succeeded []T             `json:"succeeded"`
		Failed    []batch.Failure `json:"failed,omitempty"`
	}{Succeeded: result.Succeeded, Failed: result.Failed}); err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
