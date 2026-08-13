package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/lockfile"
	"github.com/kumabox/kumabox/internal/metering"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/resourceguard"
	"github.com/kumabox/kumabox/internal/resources"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const forcedStopTimeout = 5 * time.Second

const defaultQEMUImgBinary = "qemu-img"

// Runtime coordinates VM lifecycle operations across the store, backend, and
// host-side providers.
//
// KumaBox is daemonless, so each command must reconcile persisted intent with
// the current backend process state before making lifecycle decisions.
type Runtime struct {
	vmReader      state.VMReader
	vmRecords     state.VMRecords
	vmUpdater     state.VMUpdater
	vmRestore     state.VMRestore
	operations    state.OperationState
	storeSet      StoreSet
	backend       backend.Lifecycle
	cfg           config.Config
	vmLocks       *lockfile.Locker
	resourceGuard *resourceguard.Guard
	qemuImg       *storage.QEMUImg
	network       *networkCoordinator
	storage       *storageCoordinator
}

// CreateStoppedSnapshot captures managed writable disks while holding the VM
// operation lock for the full consistency boundary.
func (r *Runtime) CreateStoppedSnapshot(ctx context.Context, ref, name string) (*snapshot.Record, error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for snapshot: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState == vmstore.ObservedStateRunning || observed.State == vmstore.StateRunning {
		return nil, fmt.Errorf("VM_RUNNING: VM %s must be stopped before snapshot", rec.Name)
	}
	if observed.State != vmstore.StateStopped {
		return nil, fmt.Errorf("VM_NOT_STOPPED: VM %s state is %s", rec.Name, observed.State)
	}
	build, err := r.storeSet.Snapshots.Reserve(ctx, name)
	if err != nil {
		return nil, err
	}
	defer build.Abort() //nolint:errcheck
	_, sizeBytes, err := snapshot.CaptureStopped(ctx, build, observed)
	if err != nil {
		return nil, err
	}
	ready, err := build.FinalizeContext(ctx, sizeBytes)
	if err != nil {
		return nil, err
	}
	if rec.Image != nil {
		if err := r.recordSnapshotImageReference(ctx, ready.ID, rec.Image.ID); err != nil {
			_, _ = r.storeSet.Snapshots.Remove(ready.ID)
			return nil, fmt.Errorf("record snapshot image reference: %w", err)
		}
	}
	return ready, nil
}

var deleteHostTap = kbnetwork.DeleteHostTap
var addCNI = kbnetwork.AddCNI
var deleteCNI = kbnetwork.DeleteCNI
var deleteCNINetNS = kbnetwork.DeleteCNINetNS
var verifyNetworkConfig = kbnetwork.VerifyConfig
var mkfsExt4 = func(path string) ([]byte, error) {
	return exec.Command("mkfs.ext4", "-F", path).CombinedOutput() //nolint:gosec
}

// New creates a Runtime backed by the configured Cloud Hypervisor backend.
func New(cfg config.Config) (*Runtime, error) {
	stores, err := resources.NewStoreSetForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("open configured resource stores: %w", err)
	}
	rt, err := NewWithBackendAndStores(stores, cloudhypervisor.NewBackend(cfg))
	if err != nil {
		return nil, err
	}
	rt.cfg = cfg
	rt.qemuImg = storage.NewQEMUImg(cfg.Storage.QEMUImgBinary)
	return rt, nil
}

// NewWithBackend creates a Runtime with an injected VM store and backend.
func NewWithBackend(store state.VMState, vmBackend backend.Lifecycle) *Runtime {
	stores := newStoreSet(store.RootDir(), store)
	rt := &Runtime{
		vmReader:      store,
		vmRecords:     store,
		vmUpdater:     store,
		vmRestore:     store,
		operations:    stores.Operations,
		storeSet:      stores,
		backend:       vmBackend,
		vmLocks:       lockfile.New(filepath.Join(store.RootDir(), "locks", "vms")),
		resourceGuard: stores.Guard,
		qemuImg:       storage.NewQEMUImg(defaultQEMUImgBinary),
	}
	rt.initNetworkCoordinator()
	return rt
}

// NewWithBackendAndStores creates a Runtime with an explicit resource-store
// composition. This is the seam used when switching metadata engines.
func NewWithBackendAndStores(stores StoreSet, vmBackend backend.Lifecycle) (*Runtime, error) {
	if stores.VM == nil {
		return nil, errors.New("runtime store set must include a VM store")
	}
	if stores.Guard == nil {
		stores.Guard = resourceguard.New(stores.VM.RootDir())
	}
	rt := &Runtime{
		vmReader:      stores.VM,
		vmRecords:     stores.VM,
		vmUpdater:     stores.VM,
		vmRestore:     stores.VM,
		operations:    stores.Operations,
		storeSet:      stores,
		backend:       vmBackend,
		vmLocks:       lockfile.New(filepath.Join(stores.VM.RootDir(), "locks", "vms")),
		resourceGuard: stores.Guard,
		qemuImg:       storage.NewQEMUImg(defaultQEMUImgBinary),
	}
	rt.initNetworkCoordinator()
	return rt, nil
}

// CreateVM creates a VM record and renders its backend configuration.
//
// Network allocation is part of creation because the rendered VMM config needs
// stable tap/MAC/IP values. If rendering fails, runtime rolls back any provider
// resources before removing the VM record.
func (r *Runtime) CreateVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	mutation, err := r.resourceGuard.BeginMutation(context.Background())
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck
	return r.createVMContext(context.Background(), req, nil)
}

func (r *Runtime) createVMContext(ctx context.Context, req vmstore.CreateRequest, metrics *lifecycleMetrics) (*vmstore.VMRecord, error) {
	if req.Image != nil && req.Image.ID != "" {
		imageLock, err := r.resourceGuard.LockEntity(ctx, resourceguard.EntityImage, req.Image.ID)
		if err != nil {
			return nil, err
		}
		defer imageLock.Release() //nolint:errcheck
	}
	rec, err := r.vmRecords.Create(req)
	if err != nil {
		return nil, err
	}
	if metrics != nil {
		metrics.bindRecord(rec)
		metrics.markImageResolved(time.Now())
	}
	if err := r.network.attachNetwork(ctx, rec); err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, err
	}
	if updated, err := r.vmReader.Inspect(rec.ID); err == nil {
		rec = updated
	}
	if metrics != nil {
		metrics.bindRecord(rec)
		metrics.markNetworkReady(time.Now())
	}
	if err := r.storage.prepare(ctx, rec); err != nil {
		r.network.rollbackNetwork(rec)
		_ = r.storage.removeManagedDirs(rec)
		_ = r.vmRecords.Delete(rec.ID)
		return nil, err
	}
	if metrics != nil {
		metrics.markStorageReady(time.Now())
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		r.network.rollbackNetwork(rec)
		_ = r.storage.removeManagedDirs(rec)
		_ = r.vmRecords.Delete(rec.ID)
		return nil, err
	}
	if err := r.recordVMImageReference(ctx, rec); err != nil {
		r.network.rollbackNetwork(rec)
		_ = r.storage.removeManagedDirs(rec)
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("record VM image reference: %w", err)
	}
	return r.applyObservation(rec), nil
}

// StartVM starts an existing VM and records backend runtime details.
//
// The backend config is rendered again immediately before start. That keeps the
// run directory recoverable after tmp cleanup and allows later phases to update
// generated metadata without mutating durable VM intent.
func (r *Runtime) StartVM(ref string) (*vmstore.VMRecord, error) {
	return r.StartVMContext(context.Background(), ref)
}

// StartVMContext starts an existing VM while holding its cross-process
// operation lock. Waiting for the lock observes ctx cancellation.
func (r *Runtime) StartVMContext(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	commandStarted := time.Now()
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, operation.KindVMStart, rec.ID)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, r.finishOperation(ctx, operationID, fmt.Errorf("lock VM %s for start: %w", rec.ID, err))
	}
	defer lock.Release() //nolint:errcheck
	metrics := newLifecycleMetrics("start", commandStarted, rec)
	metrics.markImageResolved(commandStarted)
	result, startErr := r.startVMLocked(ctx, rec.ID, metrics)
	return result, r.finishOperation(ctx, operationID, startErr)
}

func (r *Runtime) startVMLocked(ctx context.Context, ref string, metrics *lifecycleMetrics) (*vmstore.VMRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	if rec.Restore != nil {
		return nil, fmt.Errorf("VM_RESTORE_DIRTY: VM %s has an incomplete restore from snapshot %s; retry restore or delete the VM", rec.Name, rec.Restore.SnapshotID)
	}
	if rec.Hibernate != nil {
		return nil, fmt.Errorf("VM_HIBERNATED: VM %s must be restored from snapshot %s", rec.Name, rec.Hibernate.SnapshotID)
	}
	startReason := metering.ReasonBoot
	if rec.StartedAt != nil {
		startReason = metering.ReasonRestart
	}
	if metrics == nil {
		metrics = newLifecycleMetrics("start", time.Now(), rec)
	}
	metrics.bindRecord(rec)
	if err := r.network.ensureNetwork(ctx, rec); err != nil {
		if _, markErr := r.vmUpdater.SetError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	metrics.markNetworkReady(time.Now())
	if err := r.storage.prepare(ctx, rec); err != nil {
		if _, markErr := r.vmUpdater.SetError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	metrics.markStorageReady(time.Now())

	if err := r.backend.RenderConfig(rec); err != nil {
		if _, markErr := r.vmUpdater.SetError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}

	metrics.markVMMSpawned(time.Now())
	result, err := r.backend.StartVM(rec)
	if err != nil {
		if _, markErr := r.vmUpdater.SetError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	metrics.markVMMAPIReady(time.Now())
	started, err := r.vmUpdater.MarkStarted(rec.ID, result.PID, result.APISocket)
	if err != nil {
		return nil, err
	}
	// A responsive VMM API is the lifecycle boundary. Guest-agent capability
	// is checked independently by agent and exec commands.
	updated, err := r.vmUpdater.UpdatePerformance(started.ID, metrics.snapshot())
	if err != nil {
		return nil, err
	}
	r.recordComputeStart(ctx, updated, startReason)
	return r.applyObservation(updated), nil
}

// RunVM creates and starts a VM.
func (r *Runtime) RunVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	return r.RunVMContext(context.Background(), req)
}

// RunVMContext creates and starts a VM with cancellation propagated to start.
func (r *Runtime) RunVMContext(ctx context.Context, req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	metrics := newLifecycleMetrics("run", time.Now(), nil)
	rec, err := r.createVMContext(ctx, req, metrics)
	if err != nil {
		return nil, err
	}
	started, err := r.startVMWithMetrics(ctx, rec.ID, metrics)
	if err != nil {
		return nil, err
	}
	return started, nil
}

func (r *Runtime) startVMWithMetrics(ctx context.Context, ref string, metrics *lifecycleMetrics) (*vmstore.VMRecord, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for start: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	return r.startVMLocked(ctx, rec.ID, metrics)
}

// StopVM stops a running VM and updates its persisted state.
//
// Stop does not release network leases, delete tap devices, or remove provider
// records. Those resources are part of the VM's restartable identity and are
// released only by DeleteVM.
func (r *Runtime) StopVM(ref string, opts backend.StopOptions) (*vmstore.VMRecord, error) {
	return r.StopVMContext(context.Background(), ref, opts)
}

// StopVMContext stops a VM while holding its cross-process operation lock.
func (r *Runtime) StopVMContext(ctx context.Context, ref string, opts backend.StopOptions) (*vmstore.VMRecord, error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, operation.KindVMStop, rec.ID)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, r.finishOperation(ctx, operationID, fmt.Errorf("lock VM %s for stop: %w", rec.ID, err))
	}
	defer lock.Release() //nolint:errcheck
	result, stopErr := r.stopVMLocked(ctx, rec.ID, opts, metering.ReasonStopUser)
	return result, r.finishOperation(ctx, operationID, stopErr)
}

func (r *Runtime) stopVMLocked(ctx context.Context, ref string, opts backend.StopOptions, reason metering.Reason) (*vmstore.VMRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("stop VM: %w", err)
	}
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	computeOpen := observed.StartedAt != nil && observed.StoppedAt == nil
	if (observed.State == vmstore.StateRunning || observed.State == vmstore.StatePaused) &&
		observed.ObservedState != vmstore.ObservedStateRunning && observed.ObservedState != vmstore.ObservedStatePaused {
		if err := r.vmUpdater.UpdateStates([]string{observed.ID}, vmstore.StateStopped); err != nil {
			return nil, err
		}
		stopped, err := r.vmReader.Inspect(observed.ID)
		if err != nil {
			return nil, err
		}
		if computeOpen {
			r.recordComputeStop(ctx, stopped, metering.ReasonStopCrash)
		}
		_ = writeVMEvent(stopped, "backend.stop.completed", vmstore.Observation{
			State:     vmstore.ObservedStateStopped,
			Reason:    "VM was already not running",
			CheckedAt: time.Now().UTC(),
		})
		return r.applyObservation(stopped), nil
	}
	if observed.ObservedState != vmstore.ObservedStateRunning && observed.ObservedState != vmstore.ObservedStatePaused {
		return observed, nil
	}

	if _, err := r.backend.StopVM(observed, opts); err != nil {
		if _, markErr := r.vmUpdater.SetError(observed.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	if err := r.vmUpdater.UpdateStates([]string{observed.ID}, vmstore.StateStopped); err != nil {
		return nil, err
	}
	stopped, err := r.vmReader.Inspect(observed.ID)
	if err != nil {
		return nil, err
	}
	if computeOpen {
		r.recordComputeStop(ctx, stopped, reason)
	}
	_ = writeVMEvent(stopped, "backend.stop.completed", vmstore.Observation{
		State:     vmstore.ObservedStateStopped,
		Reason:    "VM stopped",
		CheckedAt: time.Now().UTC(),
	})
	return r.applyObservation(stopped), nil
}

// DeleteVM removes a VM record and KumaBox-managed resources.
//
// A running VM must be deleted with force so runtime can stop the backend first.
// Network cleanup is performed before deleting the VM record; if cleanup fails,
// the record remains available for inspect/logs/retry and the provider record is
// marked cleanup-pending.
func (r *Runtime) DeleteVM(ref string, force bool) (*vmstore.VMRecord, error) {
	return r.DeleteVMContext(context.Background(), ref, force)
}

// DeleteVMContext deletes a VM while serializing stop and cleanup under one
// operation lock.
func (r *Runtime) DeleteVMContext(ctx context.Context, ref string, force bool) (result *vmstore.VMRecord, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	operationID, err := r.beginOperation(ctx, operation.KindVMDelete, rec.ID)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for delete: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("delete VM: %w", err)
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	deleteComputeOpen := observed.StartedAt != nil && observed.StoppedAt == nil
	if observed.ObservedState == vmstore.ObservedStateRunning || observed.ObservedState == vmstore.ObservedStatePaused {
		if !force {
			return nil, fmt.Errorf("VM %s is running or paused; use --force to stop and delete", ref)
		}
		observed, err = r.stopVMLocked(ctx, rec.ID, backend.StopOptions{Force: true}, metering.ReasonDelete)
		if err != nil {
			return nil, err
		}
		if deleteComputeOpen {
			if err := r.requireComputeStop(ctx, observed, metering.ReasonDelete); err != nil {
				return nil, fmt.Errorf("record final VM usage: %w", err)
			}
		}
	}
	if observed.StartedAt != nil && observed.StoppedAt == nil {
		if err := r.vmUpdater.UpdateStates([]string{observed.ID}, vmstore.StateStopped); err != nil {
			return nil, err
		}
		observed, err = r.vmReader.Inspect(observed.ID)
		if err != nil {
			return nil, err
		}
		if err := r.requireComputeStop(ctx, observed, metering.ReasonDelete); err != nil {
			return nil, fmt.Errorf("record final VM usage: %w", err)
		}
	}

	if err := r.network.cleanupNetwork(ctx, observed); err != nil {
		return nil, err
	}
	if err := r.removeVMReferences(ctx, observed.ID); err != nil {
		return nil, fmt.Errorf("remove VM references: %w", err)
	}

	_ = writeVMEvent(observed, "backend.delete.completed", vmstore.Observation{
		State:     observed.ObservedState,
		Reason:    "VM deleted",
		CheckedAt: time.Now().UTC(),
	})
	if err := r.storage.removeManagedDirs(observed); err != nil {
		return nil, err
	}
	if err := fault.Check(ctx, fault.DeleteBeforeRecordDelete); err != nil {
		return nil, err
	}
	if err := r.vmRecords.Delete(observed.ID); err != nil {
		return nil, err
	}
	return observed, nil
}

// InspectVM returns a VM record with a fresh backend observation.
func (r *Runtime) InspectVM(ref string) (*vmstore.VMRecord, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	observed.NetworkStatus = r.network.inspectNetwork(observed)
	return observed, nil
}

// ListVMs returns all VM records with fresh backend observations.
func (r *Runtime) ListVMs() ([]*vmstore.VMRecord, error) {
	records, err := r.vmReader.List()
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		r.applyObservation(rec)
	}
	return records, nil
}

func (r *Runtime) applyObservation(rec *vmstore.VMRecord) *vmstore.VMRecord {
	if rec == nil {
		return nil
	}
	obs := r.backend.ObserveVM(rec)
	rec.ObservedState = obs.State
	rec.ObservedReason = obs.Reason
	rec.ObservedAt = &obs.CheckedAt
	if (rec.State == vmstore.StateRunning || rec.State == vmstore.StatePaused) &&
		obs.State != vmstore.ObservedStateRunning && obs.State != vmstore.ObservedStatePaused {
		_ = writeVMEvent(rec, "backend.exit.detected", obs)
	}
	return rec
}

func (r *networkCoordinator) inspectNetwork(rec *vmstore.VMRecord) *kbnetwork.InspectResult {
	if rec == nil {
		return nil
	}
	result, err := r.storeSet.Networks.InspectVM(rec.ID, rec.Name, rec.Network, rec.Networks, rec.NetworkConfigs)
	if err != nil {
		return &kbnetwork.InspectResult{
			VMID:       rec.ID,
			VMName:     rec.Name,
			Network:    rec.Network,
			Networks:   append([]string(nil), rec.Networks...),
			Interfaces: []kbnetwork.Record{},
			VMConfigs:  rec.NetworkConfigs,
			Drift:      []string{err.Error()},
		}
	}
	return result
}

func (r *networkCoordinator) attachNetwork(ctx context.Context, rec *vmstore.VMRecord) (resultErr error) {
	operationID, err := r.beginOperation(ctx, operation.KindNetworkAttach, rec.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	selections := networkSelections(rec)
	if len(selections) == 0 {
		return nil
	}
	attached := make([]kbnetwork.Config, 0, len(selections))
	for index, selection := range selections {
		allocation, err := r.attachNetworkConfig(ctx, rec, selection, index)
		if err != nil {
			r.rollbackNetworkConfigs(rec, attached)
			return err
		}
		attached = append(attached, allocation.Config)
	}
	if len(attached) == 0 {
		return nil
	}
	if _, err := r.vmRecords.SetNetworkConfigs(rec.ID, attached); err != nil {
		r.rollbackNetworkConfigs(rec, attached)
		return err
	}
	return nil
}

func (r *networkCoordinator) attachNetworkConfig(ctx context.Context, rec *vmstore.VMRecord, selection string, index int) (*kbnetwork.Allocation, error) {
	allocation, _, err := r.attachNetworkConfigWithExisting(ctx, rec, selection, index, nil)
	return allocation, err
}

func (r *networkCoordinator) attachNetworkConfigWithExisting(
	ctx context.Context,
	rec *vmstore.VMRecord,
	selection string,
	index int,
	existing *kbnetwork.Config,
) (*kbnetwork.Allocation, bool, error) {
	if kbnetwork.IsCNISelection(selection) {
		allocation, err := r.attachCNIConfig(ctx, rec, selection, index, existing)
		return allocation, false, err
	}
	if selection != "default" && selection != kbnetwork.ProviderHostTap {
		return nil, false, fmt.Errorf("unsupported network %q", selection)
	}
	// Provider state is created before the VM is rendered so Cloud Hypervisor
	// always receives a concrete tap device name. The reverse cleanup path below
	// keeps lease/index/tap state consistent if any later step fails.
	if err := config.EnsureRuntimeDirs(r.cfg); err != nil {
		return nil, false, err
	}
	networkStore, err := r.providerStore()
	if err != nil {
		return nil, false, err
	}
	previousHostState, err := networkStore.ReadHostTapState()
	if err != nil {
		return nil, false, err
	}
	if _, err := kbnetwork.EnsureHostTapWithStore(ctx, r.cfg.Runtime.RootDir, r.cfg.Network, networkStore); err != nil {
		return nil, false, err
	}
	allocator := kbnetwork.NewAllocatorWithStore(networkStore, r.cfg.Network)
	allocation, err := allocator.Allocate(kbnetwork.AllocateRequest{
		VMID:     rec.ID,
		Network:  selection,
		Index:    index,
		CPU:      rec.CPUs,
		Existing: existing,
	})
	if err != nil {
		return nil, false, err
	}
	if err := kbnetwork.AttachHostTap(allocation.Record); err != nil {
		if existing == nil {
			_ = allocator.ReleaseIP(allocation.Config.Network.IP)
		}
		return nil, false, err
	}
	if err := networkStore.UpsertRecord(allocation.Record); err != nil {
		_ = deleteHostTap(allocation.Record.TAP)
		if existing == nil {
			_ = allocator.ReleaseIP(allocation.Config.Network.IP)
		}
		return nil, false, err
	}
	hostRefAdded := existing == nil || previousHostState == nil
	if hostRefAdded {
		if err := networkStore.IncrementHostTapRef(1); err != nil {
			_ = networkStore.DeleteRecord(allocation.Record.ID)
			_ = deleteHostTap(allocation.Record.TAP)
			if existing == nil {
				_ = allocator.ReleaseIP(allocation.Config.Network.IP)
			}
			return nil, false, err
		}
	}
	return allocation, hostRefAdded, nil
}

func (r *networkCoordinator) attachCNIConfig(
	ctx context.Context,
	rec *vmstore.VMRecord,
	selection string,
	index int,
	existing *kbnetwork.Config,
) (*kbnetwork.Allocation, error) {
	if err := config.EnsureRuntimeDirs(r.cfg); err != nil {
		return nil, err
	}
	allocation, err := addCNI(ctx, r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIAddRequest{
		VMID:     rec.ID,
		Network:  selection,
		Index:    index,
		CPU:      rec.CPUs,
		Existing: existing,
	})
	if err != nil {
		return nil, err
	}
	if err := fault.Check(ctx, fault.NetworkAfterAdd); err != nil {
		rollbackErr := deleteCNI(ctx, r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIDeleteRequest{
			VMID: rec.ID, Network: selection, IfName: allocation.Record.IfName,
			TAP: allocation.Record.TAP, NetNSPath: allocation.Record.NetnsPath,
		})
		return nil, errors.Join(err, rollbackErr)
	}
	networkStore := r.storeSet.Networks
	if err := networkStore.UpsertRecord(allocation.Record); err != nil {
		_ = deleteCNI(ctx, r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIDeleteRequest{
			VMID:      rec.ID,
			Network:   selection,
			IfName:    allocation.Record.IfName,
			TAP:       allocation.Record.TAP,
			NetNSPath: allocation.Record.NetnsPath,
		})
		return nil, err
	}
	return allocation, nil
}

func (r *networkCoordinator) rollbackNetwork(rec *vmstore.VMRecord) {
	if rec == nil {
		return
	}
	r.rollbackNetworkConfigs(rec, rec.NetworkConfigs)
}

func (r *networkCoordinator) cleanupNetwork(ctx context.Context, rec *vmstore.VMRecord) (resultErr error) {
	operationID, err := r.beginOperation(ctx, operation.KindNetworkCleanup, rec.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	if rec == nil || len(rec.NetworkConfigs) == 0 {
		return nil
	}
	store := r.storeSet.Networks
	providerStore, err := r.providerStore()
	if err != nil {
		return err
	}
	allocator := kbnetwork.NewAllocatorWithStore(providerStore, r.cfg.Network)
	var cleanupErrs []error
	cniCount := countCNIConfigs(rec.NetworkConfigs)
	cniCleanupFailed := false
	for _, nc := range rec.NetworkConfigs {
		preserveCNI := false
		if nc.Backend == kbnetwork.ProviderCNI {
			preserveCNI = true
		}
		if err := cleanupNetworkConfig(ctx, store, allocator, r.cfg, rec, nc, preserveCNI); err != nil {
			// Preserve the provider record when cleanup fails. A later GC or
			// explicit retry needs the original tap/IP metadata to finish the
			// cleanup safely.
			if nc.Backend == kbnetwork.ProviderCNI {
				cniCleanupFailed = true
			}
			reason := err.Error()
			if markErr := store.MarkCleanupPending(nc.ID, reason); markErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("mark network cleanup pending for %s: %w", nc.ID, markErr))
			}
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup network %s: %w", nc.ID, err))
		}
	}
	if cniCount > 0 && !cniCleanupFailed {
		if err := deleteCNINetNS(rec.ID, cniNetNSPath(rec.NetworkConfigs)); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete CNI netns for VM %s: %w", rec.ID, err))
		}
	}
	if err := errors.Join(cleanupErrs...); err != nil {
		return fmt.Errorf("delete VM network resources: %w", err)
	}
	return nil
}

func cleanupNetworkConfig(
	ctx context.Context,
	store state.NetworkState,
	allocator *kbnetwork.Allocator,
	cfg config.Config,
	rec *vmstore.VMRecord,
	nc kbnetwork.Config,
	preserveCNINetNS bool,
) error {
	if nc.Backend == kbnetwork.ProviderCNI {
		if err := deleteCNI(ctx, cfg.Runtime.RootDir, cfg.Network, kbnetwork.CNIDeleteRequest{
			VMID:          rec.ID,
			Network:       networkSelectionForConfig(rec, nc),
			IfName:        cniIfName(nc),
			TAP:           nc.TAP,
			NetNSPath:     nc.NetnsPath,
			PreserveNetNS: preserveCNINetNS,
		}); err != nil {
			return err
		}
		if err := fault.Check(ctx, fault.NetworkAfterDelete); err != nil {
			return err
		}
		if err := store.DeleteRecord(nc.ID); err != nil {
			return fmt.Errorf("delete network provider record %s: %w", nc.ID, err)
		}
		return nil
	}
	if err := deleteHostTap(nc.TAP); err != nil {
		return fmt.Errorf("delete tap %s: %w", nc.TAP, err)
	}
	if nc.Network != nil && nc.Network.IP != "" {
		if err := allocator.ReleaseIP(nc.Network.IP); err != nil {
			return fmt.Errorf("release IP %s: %w", nc.Network.IP, err)
		}
	}
	if err := store.DecrementHostTapRef(1); err != nil {
		return err
	}
	if err := store.DeleteRecord(nc.ID); err != nil {
		return fmt.Errorf("delete network provider record %s: %w", nc.ID, err)
	}
	return nil
}

func countCNIConfigs(configs []kbnetwork.Config) int {
	count := 0
	for _, cfg := range configs {
		if cfg.Backend == kbnetwork.ProviderCNI {
			count++
		}
	}
	return count
}

func cniNetNSPath(configs []kbnetwork.Config) string {
	for _, cfg := range configs {
		if cfg.Backend == kbnetwork.ProviderCNI && cfg.NetnsPath != "" {
			return cfg.NetnsPath
		}
	}
	return ""
}

func cniIfName(nc kbnetwork.Config) string {
	if nc.IfName != "" {
		return nc.IfName
	}
	return nc.TAP
}

func networkSelections(rec *vmstore.VMRecord) []string {
	if rec == nil {
		return nil
	}
	selections := append([]string(nil), rec.Networks...)
	if len(selections) == 0 && rec.Network != "" {
		selections = append(selections, rec.Network)
	}
	filtered := selections[:0]
	for _, selection := range selections {
		if selection == "" || selection == kbnetwork.ProviderNone {
			continue
		}
		filtered = append(filtered, selection)
	}
	return filtered
}

func networkSelectionForConfig(rec *vmstore.VMRecord, nc kbnetwork.Config) string {
	if nc.NetworkName != "" {
		return nc.NetworkName
	}
	if rec != nil && rec.Network != "" && rec.Network != "multi" {
		return rec.Network
	}
	return ""
}

func (r *networkCoordinator) rollbackNetworkConfigs(rec *vmstore.VMRecord, configs []kbnetwork.Config) {
	store, err := r.providerStore()
	if err != nil {
		return
	}
	allocator := kbnetwork.NewAllocatorWithStore(store, r.cfg.Network)
	cniRemaining := countCNIConfigs(configs)
	for i := len(configs) - 1; i >= 0; i-- {
		nc := configs[i]
		if nc.Backend == kbnetwork.ProviderCNI {
			cniRemaining--
			_ = store.DeleteRecord(nc.ID)
			_ = deleteCNI(context.Background(), r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIDeleteRequest{
				VMID:          rec.ID,
				Network:       networkSelectionForConfig(rec, nc),
				IfName:        cniIfName(nc),
				TAP:           nc.TAP,
				NetNSPath:     nc.NetnsPath,
				PreserveNetNS: cniRemaining > 0,
			})
			continue
		}
		_ = store.DeleteRecord(nc.ID)
		_ = deleteHostTap(nc.TAP)
		if nc.Network != nil {
			_ = allocator.ReleaseIP(nc.Network.IP)
		}
		_ = store.DecrementHostTapRef(1)
	}
}
