package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// Runtime coordinates VM lifecycle operations across the store, backend, and
// host-side providers.
//
// KumaBox is daemonless, so each command must reconcile persisted intent with
// the current backend process state before making lifecycle decisions.
type Runtime struct {
	store   *vmstore.Store
	backend backend.Lifecycle
	cfg     config.Config
}

var deleteHostTap = kbnetwork.DeleteHostTap

// New creates a Runtime backed by the configured Cloud Hypervisor backend.
func New(cfg config.Config) *Runtime {
	rt := NewWithBackend(vmstore.New(cfg.Runtime.RootDir), cloudhypervisor.NewBackend(cfg))
	rt.cfg = cfg
	return rt
}

// NewWithBackend creates a Runtime with an injected VM store and backend.
func NewWithBackend(store *vmstore.Store, vmBackend backend.Lifecycle) *Runtime {
	return &Runtime{
		store:   store,
		backend: vmBackend,
	}
}

// CreateVM creates a VM record and renders its backend configuration.
//
// Network allocation is part of creation because the rendered VMM config needs
// stable tap/MAC/IP values. If rendering fails, runtime rolls back any provider
// resources before removing the VM record.
func (r *Runtime) CreateVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.store.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.attachNetwork(rec); err != nil {
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	if updated, err := r.store.Inspect(rec.ID); err == nil {
		rec = updated
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		r.rollbackNetwork(rec)
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	return r.applyObservation(rec), nil
}

// StartVM starts an existing VM and records backend runtime details.
//
// The backend config is rendered again immediately before start. That keeps the
// run directory recoverable after tmp cleanup and allows later phases to update
// generated metadata without mutating durable VM intent.
func (r *Runtime) StartVM(ref string) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}

	if err := r.backend.RenderConfig(rec); err != nil {
		if _, markErr := r.store.MarkError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}

	result, err := r.backend.StartVM(rec)
	if err != nil {
		if _, markErr := r.store.MarkError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	started, err := r.store.MarkRunning(rec.ID, result.PID, result.APISocket)
	if err != nil {
		return nil, err
	}
	return r.applyObservation(started), nil
}

// RunVM creates and starts a VM.
func (r *Runtime) RunVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.CreateVM(req)
	if err != nil {
		return nil, err
	}
	started, err := r.StartVM(rec.ID)
	if err != nil {
		return nil, err
	}
	return started, nil
}

// StopVM stops a running VM and updates its persisted state.
//
// Stop does not release network leases, delete tap devices, or remove provider
// records. Those resources are part of the VM's restartable identity and are
// released only by DeleteVM.
func (r *Runtime) StopVM(ref string, opts backend.StopOptions) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}

	observed := r.applyObservation(rec)
	if observed.State == vmstore.StateRunning && observed.ObservedState != vmstore.ObservedStateRunning {
		stopped, markErr := r.store.MarkStopped(observed.ID)
		if markErr != nil {
			return nil, markErr
		}
		_ = writeVMEvent(stopped, "backend.stop.completed", vmstore.Observation{
			State:     vmstore.ObservedStateStopped,
			Reason:    "VM was already not running",
			CheckedAt: time.Now().UTC(),
		})
		return r.applyObservation(stopped), nil
	}
	if observed.ObservedState != vmstore.ObservedStateRunning {
		return observed, nil
	}

	if _, err := r.backend.StopVM(observed, opts); err != nil {
		if _, markErr := r.store.MarkError(observed.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	stopped, err := r.store.MarkStopped(observed.ID)
	if err != nil {
		return nil, err
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
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState == vmstore.ObservedStateRunning {
		if !force {
			return nil, fmt.Errorf("VM %s is running; use --force to stop and delete", ref)
		}
		observed, err = r.StopVM(ref, backend.StopOptions{Force: true})
		if err != nil {
			return nil, err
		}
	}

	if err := r.cleanupNetwork(observed); err != nil {
		return nil, err
	}

	_ = writeVMEvent(observed, "backend.delete.completed", vmstore.Observation{
		State:     observed.ObservedState,
		Reason:    "VM deleted",
		CheckedAt: time.Now().UTC(),
	})
	if err := removeManagedDirs(observed); err != nil {
		return nil, err
	}
	if err := r.store.Delete(observed.ID); err != nil {
		return nil, err
	}
	return observed, nil
}

// InspectVM returns a VM record with a fresh backend observation.
func (r *Runtime) InspectVM(ref string) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	observed.NetworkStatus = r.inspectNetwork(observed)
	return observed, nil
}

// ListVMs returns all VM records with fresh backend observations.
func (r *Runtime) ListVMs() ([]*vmstore.VMRecord, error) {
	records, err := r.store.List()
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
	if rec.State == vmstore.StateRunning && obs.State != vmstore.ObservedStateRunning {
		_ = writeVMEvent(rec, "backend.exit.detected", obs)
	}
	return rec
}

func (r *Runtime) inspectNetwork(rec *vmstore.VMRecord) *kbnetwork.InspectResult {
	if rec == nil {
		return nil
	}
	result, err := kbnetwork.NewStore(r.cfg.Runtime.RootDir).InspectVM(rec.ID, rec.Name, rec.Network, rec.NetworkConfigs)
	if err != nil {
		return &kbnetwork.InspectResult{
			VMID:       rec.ID,
			VMName:     rec.Name,
			Network:    rec.Network,
			Interfaces: []kbnetwork.Record{},
			VMConfigs:  rec.NetworkConfigs,
			Drift:      []string{err.Error()},
		}
	}
	return result
}

type eventRecord struct {
	Time          time.Time             `json:"time"`
	Type          string                `json:"type"`
	VMID          string                `json:"vmId"`
	VMName        string                `json:"vmName"`
	State         vmstore.VMState       `json:"state"`
	ObservedState vmstore.ObservedState `json:"observedState"`
	Reason        string                `json:"reason,omitempty"`
	PID           int                   `json:"pid,omitempty"`
	APISocket     string                `json:"apiSocket,omitempty"`
}

func writeVMEvent(rec *vmstore.VMRecord, eventType string, obs vmstore.Observation) error {
	if rec.LogDir == "" {
		return nil
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		return fmt.Errorf("create VM log dir: %w", err)
	}

	path := filepath.Join(rec.LogDir, "events.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open events log: %w", err)
	}
	defer file.Close() //nolint:errcheck

	event := eventRecord{
		Time:          obs.CheckedAt,
		Type:          eventType,
		VMID:          rec.ID,
		VMName:        rec.Name,
		State:         rec.State,
		ObservedState: obs.State,
		Reason:        obs.Reason,
		PID:           rec.PID,
		APISocket:     rec.APISocket,
	}
	if err := json.NewEncoder(file).Encode(event); err != nil {
		return fmt.Errorf("write events log: %w", err)
	}
	return nil
}

func removeManagedDirs(rec *vmstore.VMRecord) error {
	for _, dir := range []string{rec.RunDir, rec.LogDir} {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove managed directory %s: %w", dir, err)
		}
	}
	return nil
}

func (r *Runtime) attachNetwork(rec *vmstore.VMRecord) error {
	if rec == nil || rec.Network == "" || rec.Network == kbnetwork.ProviderNone {
		return nil
	}
	if rec.Network != "default" && rec.Network != kbnetwork.ProviderHostTap {
		return fmt.Errorf("unsupported network %q", rec.Network)
	}
	// Provider state is created before the VM is rendered so Cloud Hypervisor
	// always receives a concrete tap device name. The reverse cleanup path below
	// keeps lease/index/tap state consistent if any later step fails.
	if err := config.EnsureRuntimeDirs(r.cfg); err != nil {
		return err
	}
	if _, err := kbnetwork.EnsureHostTap(context.Background(), r.cfg.Runtime.RootDir, r.cfg.Network); err != nil {
		return err
	}
	allocation, err := kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    rec.ID,
		Network: rec.Network,
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		return err
	}
	if err := kbnetwork.AttachHostTap(allocation.Record); err != nil {
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return err
	}
	networkStore := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	if err := networkStore.UpsertRecord(allocation.Record); err != nil {
		_ = deleteHostTap(allocation.Record.TAP)
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return err
	}
	if _, err := r.store.SetNetworkConfigs(rec.ID, []kbnetwork.Config{allocation.Config}); err != nil {
		_ = networkStore.DeleteRecord(allocation.Record.ID)
		_ = deleteHostTap(allocation.Record.TAP)
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return err
	}
	if err := networkStore.IncrementHostTapRef(1); err != nil {
		_, _ = r.store.SetNetworkConfigs(rec.ID, nil)
		_ = networkStore.DeleteRecord(allocation.Record.ID)
		_ = deleteHostTap(allocation.Record.TAP)
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return err
	}
	return nil
}

func (r *Runtime) rollbackNetwork(rec *vmstore.VMRecord) {
	if rec == nil {
		return
	}
	store := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	allocator := kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network)
	for _, nc := range rec.NetworkConfigs {
		_ = store.DeleteRecord(nc.ID)
		_ = deleteHostTap(nc.TAP)
		if nc.Network != nil {
			_ = allocator.ReleaseIP(nc.Network.IP)
		}
		_ = store.DecrementHostTapRef(1)
	}
}

func (r *Runtime) cleanupNetwork(rec *vmstore.VMRecord) error {
	if rec == nil || len(rec.NetworkConfigs) == 0 {
		return nil
	}
	store := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	allocator := kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network)
	var cleanupErrs []error
	for _, nc := range rec.NetworkConfigs {
		if err := cleanupNetworkConfig(store, allocator, nc); err != nil {
			// Preserve the provider record when cleanup fails. A later GC or
			// explicit retry needs the original tap/IP metadata to finish the
			// cleanup safely.
			reason := err.Error()
			if markErr := store.MarkCleanupPending(nc.ID, reason); markErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("mark network cleanup pending for %s: %w", nc.ID, markErr))
			}
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup network %s: %w", nc.ID, err))
		}
	}
	if err := errors.Join(cleanupErrs...); err != nil {
		return fmt.Errorf("delete VM network resources: %w", err)
	}
	return nil
}

func cleanupNetworkConfig(store *kbnetwork.Store, allocator *kbnetwork.Allocator, nc kbnetwork.Config) error {
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
