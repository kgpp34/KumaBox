package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// PauseVM pauses a running VM while holding its cross-process operation lock.
func (r *Runtime) PauseVM(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
	return r.transitionVMState(ctx, ref, vmstore.StatePaused)
}

// ResumeVM resumes a paused VM while holding its cross-process operation lock.
func (r *Runtime) ResumeVM(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
	return r.transitionVMState(ctx, ref, vmstore.StateRunning)
}

func (r *Runtime) transitionVMState(ctx context.Context, ref string, target vmstore.VMState) (result *vmstore.VMRecord, resultErr error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, liveStateOperation(target), rec.ID)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for %s: %w", rec.ID, target, err)
	}
	defer lock.Release() //nolint:errcheck

	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	controller, ok := r.backend.(backend.StateController)
	if !ok {
		return nil, fmt.Errorf("BACKEND_OPERATION_UNSUPPORTED: backend does not support %s", target)
	}

	switch target {
	case vmstore.StatePaused:
		if observed.ObservedState == vmstore.ObservedStatePaused {
			updated, persistErr := r.persistLiveState(observed.ID, target)
			if persistErr != nil {
				return nil, persistErr
			}
			return r.applyObservation(updated), nil
		}
		if observed.ObservedState != vmstore.ObservedStateRunning {
			return nil, fmt.Errorf("VM_NOT_RUNNING: VM %s observed state is %s", observed.Name, observed.ObservedState)
		}
		if err := controller.PauseVM(ctx, observed); err != nil {
			return nil, fmt.Errorf("pause VM %s: %w", observed.Name, err)
		}
	case vmstore.StateRunning:
		if observed.ObservedState == vmstore.ObservedStateRunning {
			updated, persistErr := r.persistLiveState(observed.ID, target)
			if persistErr != nil {
				return nil, persistErr
			}
			return r.applyObservation(updated), nil
		}
		if observed.ObservedState != vmstore.ObservedStatePaused {
			return nil, fmt.Errorf("VM_NOT_PAUSED: VM %s observed state is %s", observed.Name, observed.ObservedState)
		}
		if err := controller.ResumeVM(ctx, observed); err != nil {
			return nil, fmt.Errorf("resume VM %s: %w", observed.Name, err)
		}
	default:
		return nil, fmt.Errorf("unsupported live state transition target %s", target)
	}

	updated, err := r.persistLiveState(observed.ID, target)
	if err != nil {
		return nil, err
	}
	expected := vmstore.ObservedStatePaused
	eventType := "backend.pause.completed"
	if target == vmstore.StateRunning {
		expected = vmstore.ObservedStateRunning
		eventType = "backend.resume.completed"
	}
	updated = r.applyObservation(updated)
	if updated.ObservedState != expected {
		return nil, fmt.Errorf("BACKEND_STATE_MISMATCH: %s succeeded but backend observed state is %s", target, updated.ObservedState)
	}
	_ = writeVMEvent(updated, eventType, vmstore.Observation{
		State:     expected,
		Reason:    "VM " + string(target),
		CheckedAt: time.Now().UTC(),
	})
	return updated, nil
}

func liveStateOperation(target vmstore.VMState) string {
	switch target {
	case vmstore.StatePaused:
		return "vm.pause"
	case vmstore.StateRunning:
		return "vm.resume"
	default:
		return "vm.state-transition"
	}
}

func (r *Runtime) persistLiveState(ref string, state vmstore.VMState) (*vmstore.VMRecord, error) {
	if err := r.vmUpdater.UpdateStates([]string{ref}, state); err != nil {
		return nil, err
	}
	return r.vmReader.Inspect(ref)
}
