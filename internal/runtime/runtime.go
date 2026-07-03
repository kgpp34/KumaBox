package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type Runtime struct {
	store   *vmstore.Store
	backend backend.Lifecycle
}

// New creates a Runtime backed by the configured Cloud Hypervisor backend.
func New(cfg config.Config) *Runtime {
	return NewWithBackend(vmstore.New(cfg.Runtime.RootDir), cloudhypervisor.NewBackend(cfg))
}

// NewWithBackend creates a Runtime with an injected VM store and backend.
func NewWithBackend(store *vmstore.Store, vmBackend backend.Lifecycle) *Runtime {
	return &Runtime{
		store:   store,
		backend: vmBackend,
	}
}

// CreateVM creates a VM record and renders its backend configuration.
func (r *Runtime) CreateVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.store.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	return r.applyObservation(rec), nil
}

// StartVM starts an existing VM and records backend runtime details.
func (r *Runtime) StartVM(ref string) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
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

// DeleteVM removes a VM record and KumaBox-managed runtime directories.
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
	return r.applyObservation(rec), nil
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
