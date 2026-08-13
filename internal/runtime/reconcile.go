package runtime

import (
	"context"
	"fmt"

	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// ReconcileOperations closes operation records left running by an interrupted
// control-plane process. It only publishes success when durable state already
// proves that the operation completed; it never retries a backend action.
func (r *Runtime) ReconcileOperations(ctx context.Context) error {
	if r.operations == nil {
		return nil
	}
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return err
	}
	defer mutation.Release() //nolint:errcheck
	if err := r.operations.Reconcile(ctx, r.reconcileOperation); err != nil {
		return err
	}
	return r.ReconcileMetering(ctx)
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
	case operation.KindNetworkResize:
		return r.requireVMExists(record)
	case operation.KindDiskAttach, operation.KindDiskDetach:
		return r.requireVMExists(record)
	case operation.KindFilesystemAttach, operation.KindFilesystemDetach:
		return r.requireVMExists(record)
	case operation.KindPCIAttach, operation.KindPCIDetach:
		return r.requireVMExists(record)
	case operation.KindSnapshotCreateRun:
		return r.requireSnapshotForVM(ctx, record, record.RelatedID)
	case operation.KindSnapshotCloneNative:
		return r.requireVMRestore(record)
	case operation.KindSnapshotRestoreVM:
		return r.requireVMRestore(record)
	case operation.KindVMHibernate:
		return r.requireSnapshotForVM(ctx, record, record.RelatedID)
	case operation.KindSnapshotRestoreDisk:
		return r.requireVMExists(record)
	default:
		return fmt.Errorf("OPERATION_KIND_UNKNOWN: %s", record.Kind)
	}
}

func (r *Runtime) requireVMExists(record operation.Record) error {
	if _, err := r.vmReader.Inspect(record.ResourceID); err != nil {
		return fmt.Errorf("SNAPSHOT_RESTORE_INCOMPLETE: restored VM %s is unavailable: %w", record.ResourceID, err)
	}
	return nil
}

func (r *Runtime) requireVMRestore(record operation.Record) error {
	rec, err := r.vmReader.Inspect(record.ResourceID)
	if err != nil {
		return err
	}
	if rec.LastRestore != nil && rec.LastRestore.SnapshotID == record.RelatedID {
		return nil
	}
	if rec.SnapshotDependency != nil && rec.SnapshotDependency.SnapshotID == record.RelatedID {
		return nil
	}
	return fmt.Errorf("SNAPSHOT_RESTORE_INCOMPLETE: VM %s has no completed restore from %s", rec.ID, record.RelatedID)
}

func (r *Runtime) requireSnapshotForVM(ctx context.Context, record operation.Record, snapshotRef string) error {
	if r.storeSet.Snapshots == nil {
		return fmt.Errorf("SNAPSHOT_RECONCILIATION_UNAVAILABLE: snapshot state is not configured")
	}
	snapshots, err := r.storeSet.Snapshots.Scan()
	if err != nil {
		return err
	}
	for _, candidate := range snapshots {
		if candidate == nil || candidate.State != snapshot.StateReady {
			continue
		}
		if candidate.ID != snapshotRef && candidate.Name != snapshotRef {
			continue
		}
		manifest, err := r.storeSet.Snapshots.PeekManifest(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if manifest.Source.VMID == record.ResourceID {
			return nil
		}
	}
	return fmt.Errorf("SNAPSHOT_OPERATION_INCOMPLETE: no ready snapshot %s for VM %s", snapshotRef, record.ResourceID)
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
