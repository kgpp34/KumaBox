package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/metering"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vm"
)

// PauseVM pauses a running VM while holding its cross-process operation lock.
func (r *Runtime) PauseVM(ctx context.Context, ref string) (*vm.VMRecord, error) {
	return r.transitionVMState(ctx, ref, vm.StatePaused)
}

// ResumeVM resumes a paused VM while holding its cross-process operation lock.
func (r *Runtime) ResumeVM(ctx context.Context, ref string) (*vm.VMRecord, error) {
	return r.transitionVMState(ctx, ref, vm.StateRunning)
}

func (r *Runtime) transitionVMState(ctx context.Context, ref string, target vm.VMState) (result *vm.VMRecord, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

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
	case vm.StatePaused:
		if observed.ObservedState == vm.ObservedStatePaused {
			updated, persistErr := r.persistLiveState(observed.ID, target)
			if persistErr != nil {
				return nil, persistErr
			}
			return r.applyObservation(updated), nil
		}
		if observed.ObservedState != vm.ObservedStateRunning {
			return nil, fmt.Errorf("VM_NOT_RUNNING: VM %s observed state is %s", observed.Name, observed.ObservedState)
		}
		if err := controller.PauseVM(ctx, observed); err != nil {
			return nil, fmt.Errorf("pause VM %s: %w", observed.Name, err)
		}
	case vm.StateRunning:
		if observed.ObservedState == vm.ObservedStateRunning {
			updated, persistErr := r.persistLiveState(observed.ID, target)
			if persistErr != nil {
				return nil, persistErr
			}
			return r.applyObservation(updated), nil
		}
		if observed.ObservedState != vm.ObservedStatePaused {
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
	if target == vm.StatePaused {
		r.recordComputeStop(ctx, updated, metering.ReasonPause)
	} else {
		r.recordComputeStart(ctx, updated, metering.ReasonResume)
	}
	expected := vm.ObservedStatePaused
	eventType := "backend.pause.completed"
	if target == vm.StateRunning {
		expected = vm.ObservedStateRunning
		eventType = "backend.resume.completed"
	}
	updated = r.applyObservation(updated)
	if updated.ObservedState != expected {
		return nil, fmt.Errorf("BACKEND_STATE_MISMATCH: %s succeeded but backend observed state is %s", target, updated.ObservedState)
	}
	_ = writeVMEvent(updated, eventType, vm.Observation{
		State:     expected,
		Reason:    "VM " + string(target),
		CheckedAt: time.Now().UTC(),
	})
	return updated, nil
}

func liveStateOperation(target vm.VMState) string {
	switch target {
	case vm.StatePaused:
		return operation.KindVMPause
	case vm.StateRunning:
		return operation.KindVMResume
	default:
		return "vm.state-transition"
	}
}

func (r *Runtime) persistLiveState(ref string, state vm.VMState) (*vm.VMRecord, error) {
	if err := r.vmUpdater.UpdateStates([]string{ref}, state); err != nil {
		return nil, err
	}
	return r.vmReader.Inspect(ref)
}
