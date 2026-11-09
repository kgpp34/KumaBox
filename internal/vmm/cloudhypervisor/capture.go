package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

var _ vmm.CaptureRecoverer = (*Driver)(nil)

// RecoverCapture settles a journaled capture under the caller's sandbox lock.
// The long info request drains any dump still executing in the monitor before
// resume. A missing intent leaves an intentionally paused guest untouched.
func (d *Driver) RecoverCapture(ctx context.Context, id types.SandboxID, generation uint64) (recovered bool, returnErr error) {
	intent, err := d.paths.ReadCapture(id)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.Join(vmm.ErrCaptureRecoveryPending, err)
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(vmm.ErrCaptureRecoveryPending, returnErr)
		}
	}()
	process := intent.Process
	alive, err := verifyProcess(process)
	if err != nil {
		return false, err
	}
	if !alive {
		return true, d.paths.ClearCapture(id)
	}
	if process.Generation != generation {
		return false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("capture belongs to another VMM generation"))
	}
	if intent.Action == vmm.CaptureStop {
		if err := d.Stop(ctx, process, types.StopOptions{Force: true}); err != nil {
			return false, err
		}
		return true, d.paths.ClearCapture(id)
	}
	info, err := d.queryInfoTimeout(ctx, process.APISocket, snapshotTimeout)
	if err != nil {
		return false, fmt.Errorf("wait for interrupted capture: %w", err)
	}
	alive, err = verifyProcess(process)
	if err != nil {
		return false, err
	}
	if !alive {
		return true, d.paths.ClearCapture(id)
	}
	switch info.State {
	case "Paused":
		if err := d.snapshotAction(ctx, process.APISocket, "vm.resume", nil, snapshotTimeout); err != nil {
			return false, fmt.Errorf("resume captured guest: %w", err)
		}
	case "Running":
	default:
		return false, fmt.Errorf("captured guest is %s; retain capture intent for recovery", info.State)
	}
	return true, d.paths.ClearCapture(id)
}

// finishCapture retains the journal if a bounded recovery cannot complete.
// Caller cancellation is detached only for settling the already paused guest.
func (d *Driver) finishCapture(ctx context.Context, process vmm.Process) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	_, err := d.RecoverCapture(recoveryCtx, process.SandboxID, process.Generation)
	return err
}
