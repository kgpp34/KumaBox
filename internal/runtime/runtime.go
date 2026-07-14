package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/lockfile"
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
	vmLocks *lockfile.Locker
}

var deleteHostTap = kbnetwork.DeleteHostTap
var addCNI = kbnetwork.AddCNI
var deleteCNI = kbnetwork.DeleteCNI
var deleteCNINetNS = kbnetwork.DeleteCNINetNS
var mkfsExt4 = func(path string) ([]byte, error) {
	return exec.Command("mkfs.ext4", "-F", path).CombinedOutput() //nolint:gosec
}

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
		vmLocks: lockfile.New(filepath.Join(store.RootDir(), "locks", "vms")),
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
	if err := prepareStorage(rec, r.store.RootDir()); err != nil {
		r.rollbackNetwork(rec)
		_ = removeManagedDirs(rec, r.store.RootDir())
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		r.rollbackNetwork(rec)
		_ = removeManagedDirs(rec, r.store.RootDir())
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
	return r.StartVMContext(context.Background(), ref)
}

// StartVMContext starts an existing VM while holding its cross-process
// operation lock. Waiting for the lock observes ctx cancellation.
func (r *Runtime) StartVMContext(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for start: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	return r.startVMLocked(ctx, rec.ID)
}

func (r *Runtime) startVMLocked(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	if err := prepareStorage(rec, r.store.RootDir()); err != nil {
		if _, markErr := r.store.MarkError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}

	if err := r.backend.RenderConfig(rec); err != nil {
		if _, markErr := r.store.MarkError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
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
	return r.RunVMContext(context.Background(), req)
}

// RunVMContext creates and starts a VM with cancellation propagated to start.
func (r *Runtime) RunVMContext(ctx context.Context, req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.CreateVM(req)
	if err != nil {
		return nil, err
	}
	started, err := r.StartVMContext(ctx, rec.ID)
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
	return r.StopVMContext(context.Background(), ref, opts)
}

// StopVMContext stops a VM while holding its cross-process operation lock.
func (r *Runtime) StopVMContext(ctx context.Context, ref string, opts backend.StopOptions) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for stop: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	return r.stopVMLocked(ctx, rec.ID, opts)
}

func (r *Runtime) stopVMLocked(ctx context.Context, ref string, opts backend.StopOptions) (*vmstore.VMRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("stop VM: %w", err)
	}
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
	return r.DeleteVMContext(context.Background(), ref, force)
}

// DeleteVMContext deletes a VM while serializing stop and cleanup under one
// operation lock.
func (r *Runtime) DeleteVMContext(ctx context.Context, ref string, force bool) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for delete: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("delete VM: %w", err)
	}
	rec, err = r.store.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState == vmstore.ObservedStateRunning {
		if !force {
			return nil, fmt.Errorf("VM %s is running; use --force to stop and delete", ref)
		}
		observed, err = r.stopVMLocked(ctx, rec.ID, backend.StopOptions{Force: true})
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
	if err := removeManagedDirs(observed, r.store.RootDir()); err != nil {
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
	result, err := kbnetwork.NewStore(r.cfg.Runtime.RootDir).InspectVM(rec.ID, rec.Name, rec.Network, rec.Networks, rec.NetworkConfigs)
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

func removeManagedDirs(rec *vmstore.VMRecord, rootDir string) error {
	storageDir := filepath.Join(rootDir, "storage", "vms", rec.ID)
	for _, dir := range []string{rec.RunDir, rec.LogDir, storageDir} {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove managed directory %s: %w", dir, err)
		}
	}
	return nil
}

func prepareStorage(rec *vmstore.VMRecord, rootDir string) error {
	if err := vmstore.ValidateStorageContract(rec, rootDir); err != nil {
		return err
	}
	for _, cfg := range rec.StorageConfigs {
		switch cfg.EffectiveRole() {
		case vmstore.StorageRoleLayer:
			if cfg.Path == "" {
				return fmt.Errorf("storage layer %s path must not be empty", cfg.ID)
			}
			info, err := os.Stat(cfg.Path)
			if err != nil {
				return fmt.Errorf("stat storage layer %s: %w", cfg.ID, err)
			}
			if info.IsDir() {
				return fmt.Errorf("storage layer %s must be a file: %s", cfg.ID, cfg.Path)
			}
		case vmstore.StorageRoleCOW:
			if err := prepareCOW(cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareCOW(cfg vmstore.StorageConfig) error {
	if cfg.Path == "" {
		return fmt.Errorf("COW storage path must not be empty")
	}
	sizeBytes := cfg.EffectiveVirtualSize()
	if sizeBytes <= 0 {
		return fmt.Errorf("COW storage %s size must be positive", cfg.ID)
	}
	if info, err := os.Stat(cfg.Path); err == nil && info.Mode().IsRegular() && info.Size() == sizeBytes {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat COW storage %s: %w", cfg.ID, err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return fmt.Errorf("create COW storage dir: %w", err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create COW storage %s: %w", cfg.ID, err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("size COW storage %s: %w", cfg.ID, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close COW storage %s: %w", cfg.ID, err)
	}
	out, err := mkfsExt4(cfg.Path)
	if err != nil {
		_ = os.Remove(cfg.Path)
		return fmt.Errorf("mkfs.ext4 COW storage %s: %w: %s", cfg.ID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Runtime) attachNetwork(rec *vmstore.VMRecord) error {
	selections := networkSelections(rec)
	if len(selections) == 0 {
		return nil
	}
	attached := make([]kbnetwork.Config, 0, len(selections))
	for index, selection := range selections {
		allocation, err := r.attachNetworkConfig(rec, selection, index)
		if err != nil {
			rollbackNetworkConfigs(rec, r.cfg, attached)
			return err
		}
		attached = append(attached, allocation.Config)
	}
	if len(attached) == 0 {
		return nil
	}
	if _, err := r.store.SetNetworkConfigs(rec.ID, attached); err != nil {
		rollbackNetworkConfigs(rec, r.cfg, attached)
		return err
	}
	return nil
}

func (r *Runtime) attachNetworkConfig(rec *vmstore.VMRecord, selection string, index int) (*kbnetwork.Allocation, error) {
	if kbnetwork.IsCNISelection(selection) {
		return r.attachCNIConfig(rec, selection, index)
	}
	if selection != "default" && selection != kbnetwork.ProviderHostTap {
		return nil, fmt.Errorf("unsupported network %q", selection)
	}
	// Provider state is created before the VM is rendered so Cloud Hypervisor
	// always receives a concrete tap device name. The reverse cleanup path below
	// keeps lease/index/tap state consistent if any later step fails.
	if err := config.EnsureRuntimeDirs(r.cfg); err != nil {
		return nil, err
	}
	if _, err := kbnetwork.EnsureHostTap(context.Background(), r.cfg.Runtime.RootDir, r.cfg.Network); err != nil {
		return nil, err
	}
	allocation, err := kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    rec.ID,
		Network: selection,
		Index:   index,
		CPU:     rec.CPUs,
	})
	if err != nil {
		return nil, err
	}
	if err := kbnetwork.AttachHostTap(allocation.Record); err != nil {
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return nil, err
	}
	networkStore := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	if err := networkStore.UpsertRecord(allocation.Record); err != nil {
		_ = deleteHostTap(allocation.Record.TAP)
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return nil, err
	}
	if err := networkStore.IncrementHostTapRef(1); err != nil {
		_ = networkStore.DeleteRecord(allocation.Record.ID)
		_ = deleteHostTap(allocation.Record.TAP)
		_ = kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network).ReleaseIP(allocation.Config.Network.IP)
		return nil, err
	}
	return allocation, nil
}

func (r *Runtime) attachCNIConfig(rec *vmstore.VMRecord, selection string, index int) (*kbnetwork.Allocation, error) {
	if err := config.EnsureRuntimeDirs(r.cfg); err != nil {
		return nil, err
	}
	allocation, err := addCNI(context.Background(), r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIAddRequest{
		VMID:    rec.ID,
		Network: selection,
		Index:   index,
		CPU:     rec.CPUs,
	})
	if err != nil {
		return nil, err
	}
	networkStore := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	if err := networkStore.UpsertRecord(allocation.Record); err != nil {
		_ = deleteCNI(context.Background(), r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIDeleteRequest{
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

func (r *Runtime) rollbackNetwork(rec *vmstore.VMRecord) {
	if rec == nil {
		return
	}
	rollbackNetworkConfigs(rec, r.cfg, rec.NetworkConfigs)
}

func (r *Runtime) cleanupNetwork(rec *vmstore.VMRecord) error {
	if rec == nil || len(rec.NetworkConfigs) == 0 {
		return nil
	}
	store := kbnetwork.NewStore(r.cfg.Runtime.RootDir)
	allocator := kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network)
	var cleanupErrs []error
	cniCount := countCNIConfigs(rec.NetworkConfigs)
	cniCleanupFailed := false
	for _, nc := range rec.NetworkConfigs {
		preserveCNI := false
		if nc.Backend == kbnetwork.ProviderCNI {
			preserveCNI = true
		}
		if err := cleanupNetworkConfig(context.Background(), store, allocator, r.cfg, rec, nc, preserveCNI); err != nil {
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
	store *kbnetwork.Store,
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

func rollbackNetworkConfigs(rec *vmstore.VMRecord, cfg config.Config, configs []kbnetwork.Config) {
	store := kbnetwork.NewStore(cfg.Runtime.RootDir)
	allocator := kbnetwork.NewAllocator(cfg.Runtime.RootDir, cfg.Network)
	cniRemaining := countCNIConfigs(configs)
	for i := len(configs) - 1; i >= 0; i-- {
		nc := configs[i]
		if nc.Backend == kbnetwork.ProviderCNI {
			cniRemaining--
			_ = store.DeleteRecord(nc.ID)
			_ = deleteCNI(context.Background(), cfg.Runtime.RootDir, cfg.Network, kbnetwork.CNIDeleteRequest{
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
