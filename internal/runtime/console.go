package runtime

import (
	"context"
	"fmt"
	"io"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func (r *Runtime) OpenConsole(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState != vmstore.ObservedStateRunning {
		return nil, fmt.Errorf("VM_NOT_RUNNING: VM %s is not running", rec.Name)
	}
	controller, ok := r.backend.(backend.ConsoleController)
	if !ok {
		return nil, fmt.Errorf("BACKEND_OPERATION_UNSUPPORTED: backend does not support console")
	}
	return controller.OpenConsole(ctx, observed)
}
