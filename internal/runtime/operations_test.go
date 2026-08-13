package runtime

import (
	"errors"
	"testing"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/operation"
)

func TestFinishOperationPreservesInterruptedIntent(t *testing.T) {
	rt := &Runtime{operations: operation.New(t.TempDir())}
	id, err := rt.beginOperation(t.Context(), operation.KindVMDelete, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	interrupted := fault.Interrupt(fault.DeleteBeforeRecordDelete)
	if err := rt.finishOperation(t.Context(), id, interrupted); !errors.Is(err, fault.ErrInterrupted) {
		t.Fatalf("finishOperation() error = %v, want ErrInterrupted", err)
	}
	recoverable, err := rt.operations.Recoverable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != id || recoverable[0].Status != operation.StatusRunning {
		t.Fatalf("recoverable operations = %+v", recoverable)
	}
}
