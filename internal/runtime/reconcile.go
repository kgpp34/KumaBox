package runtime

import (
	"context"
	"fmt"

	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// ReconcileOperations closes operation records left running by an interrupted
// control-plane process. It only publishes success when durable state already
// proves that the operation completed; it never retries a backend action.
func (r *Runtime) ReconcileOperations(ctx context.Context) error {
	if r.operations == nil {
		return nil
	}
	return r.operations.Reconcile(ctx, r.reconcileOperation)
}

func (r *Runtime) reconcileOperation(ctx context.Context, record operation.Record) error {
	switch record.Kind {
	case operation.KindVMStart:
		return r.requireVMState(record, vmstore.ObservedStateRunning)
	case operation.KindVMStop:
		return r.requireVMState(record, vmstore.ObservedStateStopped)
	case operation.KindVMPause:
		return r.requireVMState(record, vmstore.ObservedStatePaused)
	case operation.KindVMResume:
		return r.requireVMState(record, vmstore.ObservedStateRunning)
	case operation.KindVMDelete:
		if _, err := r.vmReader.Inspect(record.ResourceID); err != nil {
			return nil
		}
		return fmt.Errorf("VM_DELETE_INCOMPLETE: VM %s still exists", record.ResourceID)
	case operation.KindNetworkAttach:
		return r.requireNetworkAttached(record)
	case operation.KindNetworkCleanup:
		return r.requireNetworkClean(record)
	case operation.KindSnapshotCreateRun, operation.KindSnapshotCloneNative,
		operation.KindSnapshotRestoreDisk, operation.KindSnapshotRestoreVM,
		operation.KindVMHibernate:
		return fmt.Errorf("OPERATION_RECONCILIATION_UNSUPPORTED: %s requires snapshot-specific inspection", record.Kind)
	default:
		return fmt.Errorf("OPERATION_KIND_UNKNOWN: %s", record.Kind)
	}
}

func (r *Runtime) requireNetworkAttached(record operation.Record) error {
	if r.storeSet.Networks == nil {
		return fmt.Errorf("NETWORK_RECONCILIATION_UNAVAILABLE: network state is not configured")
	}
	result, err := r.storeSet.Networks.Inspect(record.ResourceID)
	if err != nil {
		return err
	}
	if len(result.Interfaces) == 0 {
		return fmt.Errorf("NETWORK_ATTACH_INCOMPLETE: VM %s has no provider interface", record.ResourceID)
	}
	return nil
}

func (r *Runtime) requireNetworkClean(record operation.Record) error {
	if r.storeSet.Networks == nil {
		return fmt.Errorf("NETWORK_RECONCILIATION_UNAVAILABLE: network state is not configured")
	}
	result, err := r.storeSet.Networks.Inspect(record.ResourceID)
	if err != nil {
		return err
	}
	if len(result.Interfaces) != 0 {
		return fmt.Errorf("NETWORK_CLEANUP_INCOMPLETE: VM %s still has %d provider interface(s)", record.ResourceID, len(result.Interfaces))
	}
	return nil
}

func (r *Runtime) requireVMState(record operation.Record, expected vmstore.ObservedState) error {
	rec, err := r.vmReader.Inspect(record.ResourceID)
	if err != nil {
		return err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState != expected {
		return fmt.Errorf("VM_STATE_MISMATCH: VM %s is %s, want %s", observed.ID, observed.ObservedState, expected)
	}
	return nil
}
