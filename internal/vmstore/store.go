package vmstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

// Store serializes access to the VM index for one backend.
//
// The current implementation uses a single JSON index guarded by flock. This is
// sufficient for the daemonless CLI model: each command can safely update
// records without requiring a resident coordinator process.
type Store struct {
	rootDir string
	engine  meta.MetaEngine
}

var vmIndexCollection = meta.NewCollection[vmIndex]("vms", vmIndexTable)

// New returns a VM store rooted under rootDir.
//
// The store path is backend-scoped so future backends can maintain independent
// indexes without changing the VMRecord shape.
func New(rootDir string) *Store {
	backendDir := filepath.Join(rootDir, "backends", backendCloudHypervisor)
	engine := mustOpenEngine(metajson.Namespace{
		Name:     "vms",
		FilePath: filepath.Join(backendDir, "index.json"),
		LockPath: filepath.Join(backendDir, "index.lock"),
		Codec:    indexCodec{},
	})
	return NewWithEngine(rootDir, engine)
}

// NewWithEngine creates a VM store with an injected metadata engine.
func NewWithEngine(rootDir string, engine meta.MetaEngine) *Store {
	return &Store{rootDir: rootDir, engine: engine}
}

// MetadataEngine exposes the store's persistence boundary to migration tools.
// Runtime code should use the VM state capability instead.
func (s *Store) MetadataEngine() meta.MetaEngine { return s.engine }

func mustOpenEngine(namespace metajson.Namespace) meta.MetaEngine {
	engine, err := metajson.Open(namespace)
	if err != nil {
		panic(fmt.Sprintf("open VM metadata engine: %v", err))
	}
	return engine
}

// CreateRequest is the normalized intent needed to create a VM record.
//
// Paths are resolved to absolute paths before persistence. The request does not
// create disks, render VMM config, or allocate network resources; runtime code
// coordinates those side effects around store.Create.
type CreateRequest struct {
	Name           string
	RootDisk       string
	Kernel         string
	Initrd         string
	KernelCmdline  string
	Firmware       string
	Image          *ImageRef
	CPUs           int
	MemoryBytes    int64
	Network        string
	Networks       []string
	StorageConfigs []StorageConfig
	DataDisks      []DataDiskRequest
	SharedMemory   bool
	RunDir         string
	LogDir         string
}

// Create validates and inserts a VM record.
//
// Name uniqueness is enforced inside the store lock. On success the returned
// record is a defensive copy and may be mutated by the caller without changing
// the stored index.
func (s *Store) Create(req CreateRequest) (*VMRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *VMRecord
	err := s.update(func(idx *vmIndex) error {
		if _, ok := idx.Names[req.Name]; ok {
			return fmt.Errorf("%w: %s", ErrNameConflict, req.Name)
		}

		id, err := newID()
		if err != nil {
			return err
		}
		for {
			if _, exists := idx.VMs[id]; !exists {
				break
			}
			id, err = newID()
			if err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		rec, err := newRecord(id, req, s.rootDir, now)
		if err != nil {
			return fmt.Errorf("create VM record: %w", err)
		}
		if err := ValidateStorageContract(rec, s.rootDir); err != nil {
			return err
		}

		idx.VMs[id] = rec
		idx.Names[req.Name] = id
		created = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return created, nil
}

// Inspect returns a VM by ID or name.
//
// The returned record is a defensive copy. Callers that want live backend
// information should use runtime.InspectVM, which overlays an Observation.
func (s *Store) Inspect(ref string) (*VMRecord, error) {
	var rec *VMRecord
	err := s.withIndex(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec = cloneRecord(idx.VMs[id])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Delete removes a VM record from the index.
//
// Delete intentionally affects only the VM index. Runtime.DeleteVM is
// responsible for stopping VMMs and cleaning run/log/network resources before
// calling this method.
func (s *Store) Delete(ref string) error {
	return s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		if rec != nil {
			delete(idx.Names, rec.Name)
		}
		delete(idx.VMs, id)
		return nil
	})
}

// UpdateStates applies one state update to every existing VM in refs in a
// single metadata transaction.
func (s *Store) UpdateStates(refs []string, state VMState) error {
	if len(refs) == 0 {
		return nil
	}
	return s.update(func(idx *vmIndex) error {
		now := time.Now().UTC()
		for _, ref := range refs {
			id, err := idx.resolve(ref)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			rec := idx.VMs[id]
			rec.State = state
			rec.UpdatedAt = now
			if state == StateStopped {
				rec.PID = 0
				rec.APISocket = ""
				rec.Error = ""
				rec.SnapshotDependency = nil
				rec.StoppedAt = &now
			}
		}
		return nil
	})
}

// MarkStarted records backend process identity after a successful start.
//
// For cloud-image boots, marking running also flips FirstBooted so subsequent
// starts do not regenerate one-shot first-boot metadata unexpectedly.
func (s *Store) MarkStarted(ref string, pid int, apiSocket string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateRunning
		rec.PID = pid
		rec.APISocket = apiSocket
		rec.Error = ""
		rec.SnapshotDependency = nil
		rec.Hibernate = nil
		rec.StartedAt = &now
		if rec.Metadata != nil {
			rec.FirstBooted = true
		}
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdatePerformance persists the latest lifecycle timing after the VM has
// reached the product readiness boundary.
func (s *Store) UpdatePerformance(ref string, metrics PerformanceMetrics) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		rec.Performance = &metrics
		rec.UpdatedAt = time.Now().UTC()
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// BeginRestore writes the recovery marker before any writable disk is
// replaced. Repeated calls deliberately refresh the marker so restore is the
// recovery path for an interrupted prior attempt.
func (s *Store) BeginRestore(ref, snapshotID, mode string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateStopped
		rec.PID = 0
		rec.APISocket = ""
		rec.Error = ""
		rec.Restore = &RestoreStatus{
			SnapshotID: snapshotID,
			Mode:       mode,
			State:      "dirty",
			StartedAt:  now,
			UpdatedAt:  now,
		}
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	return updated, err
}

// FailRestore quarantines a VM after the destructive restore boundary.
// The restore marker is retained so start cannot boot mixed-generation state.
func (s *Store) FailRestore(ref, message string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateError
		rec.PID = 0
		rec.APISocket = ""
		rec.Error = message
		if rec.Restore == nil {
			rec.Restore = &RestoreStatus{State: "dirty", StartedAt: now}
		}
		rec.Restore.State = "failed"
		rec.Restore.Error = message
		rec.Restore.UpdatedAt = now
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	return updated, err
}

// CompleteRestore atomically publishes restored process identity and
// the phase timings collected during the restore or clone transaction.
func (s *Store) CompleteRestore(ref string, pid int, apiSocket string, duration time.Duration, metrics *RestoreResult) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		if rec.Restore == nil {
			return errors.New("VM_RESTORE_STATE_MISSING: restore transaction is not active")
		}
		now := time.Now().UTC()
		rec.State = StateRunning
		rec.PID = pid
		rec.APISocket = apiSocket
		rec.Error = ""
		lastRestore := &RestoreResult{
			SnapshotID:  rec.Restore.SnapshotID,
			Mode:        rec.Restore.Mode,
			DurationMs:  duration.Milliseconds(),
			CompletedAt: now,
		}
		if metrics != nil {
			lastRestore.NativeStageDurationMs = metrics.NativeStageDurationMs
			lastRestore.DiskStageDurationMs = metrics.DiskStageDurationMs
			lastRestore.DiskCommitDurationMs = metrics.DiskCommitDurationMs
			lastRestore.BackendRestoreDurationMs = metrics.BackendRestoreDurationMs
			lastRestore.IdentityDurationMs = metrics.IdentityDurationMs
			lastRestore.ReadinessDurationMs = metrics.ReadinessDurationMs
		}
		rec.LastRestore = lastRestore
		if rec.Restore.Mode == "ondemand" || rec.Restore.Mode == "mmap" {
			rec.SnapshotDependency = &SnapshotDependency{
				SnapshotID: rec.Restore.SnapshotID,
				Mode:       rec.Restore.Mode,
				Since:      now,
			}
		} else {
			rec.SnapshotDependency = nil
		}
		rec.Restore = nil
		rec.Hibernate = nil
		rec.StartedAt = &now
		rec.StoppedAt = nil
		if rec.Metadata != nil {
			rec.FirstBooted = true
		}
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	return updated, err
}

// CompleteHibernate publishes the durable snapshot linkage only after the VMM
// has terminated. Network and storage identity remain allocated for wake.
func (s *Store) CompleteHibernate(ref, snapshotID string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateStopped
		rec.PID = 0
		rec.APISocket = ""
		rec.Error = ""
		rec.SnapshotDependency = nil
		rec.Hibernate = &HibernateStatus{SnapshotID: snapshotID, CreatedAt: now}
		rec.StoppedAt = &now
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	return updated, err
}

// SetError records a lifecycle failure while preserving the VM record.
//
// Keeping the record allows inspect, logs, and delete cleanup to work after a
// failed render/start/stop operation.
func (s *Store) SetError(ref string, message string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateError
		rec.Error = message
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// SetNetworkConfigs stores the VM-side view of allocated network attachments.
//
// Provider records and leases live in the network store. Keeping a copy here
// lets runtime render Cloud Hypervisor config even if provider inspection later
// reports drift.
func (s *Store) SetNetworkConfigs(ref string, configs []kbnetwork.Config) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.NetworkConfigs = cloneNetworkConfigs(configs)
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) SetAttachedDisks(ref string, disks []AttachedDisk) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		rec.AttachedDisks = append([]AttachedDisk(nil), disks...)
		rec.UpdatedAt = time.Now().UTC()
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) SetAttachedFilesystems(ref string, filesystems []AttachedFilesystem) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		rec.AttachedFilesystems = append([]AttachedFilesystem(nil), filesystems...)
		rec.UpdatedAt = time.Now().UTC()
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// List returns all VM records sorted by creation time.
//
// Each element is a defensive copy. Runtime.ListVMs may update observations on
// these copies without changing persisted state.
func (s *Store) List() ([]*VMRecord, error) {
	var records []*VMRecord
	err := s.withIndex(func(idx *vmIndex) error {
		records = make([]*VMRecord, 0, len(idx.VMs))
		for _, rec := range idx.VMs {
			records = append(records, cloneRecord(rec))
		}
		sort.Slice(records, func(i, j int) bool {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// RootDir returns the durable state root used by this store.
func (s *Store) RootDir() string {
	return s.rootDir
}

func (s *Store) withIndex(fn func(*vmIndex) error) error {
	ctx := context.Background()
	return s.engine.View(ctx, []meta.Namespace{"vms"}, func(reader meta.Reader) error {
		idx, err := s.readIndex(ctx, reader)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func (s *Store) update(fn func(*vmIndex) error) error {
	ctx := context.Background()
	return s.engine.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		idx, err := s.readIndex(ctx, writer)
		if err != nil {
			return err
		}
		if err := fn(idx); err != nil {
			return err
		}
		return vmIndexCollection.Upsert(ctx, writer, vmIndexRecord, idx)
	})
}

func (s *Store) readIndex(ctx context.Context, reader meta.Reader) (*vmIndex, error) {
	idx, err := vmIndexCollection.Get(ctx, reader, vmIndexRecord)
	if errors.Is(err, meta.ErrNotFound) {
		idx = &vmIndex{}
	} else if err != nil {
		return nil, fmt.Errorf("read VM index: %w", err)
	}
	idx.init()
	for id, rec := range idx.VMs {
		if err := ValidateStorageContract(rec, s.rootDir); err != nil {
			return nil, fmt.Errorf("validate VM %s storage: %w", id, err)
		}
	}
	return idx, nil
}

func validateCreateRequest(req CreateRequest) error {
	if req.Name == "" {
		return errors.New("name must not be empty")
	}
	if req.RootDisk == "" && len(req.StorageConfigs) == 0 {
		return errors.New("root disk must not be empty")
	}
	if req.Firmware == "" && req.Kernel == "" {
		return errors.New("kernel must not be empty for direct boot")
	}
	if req.Firmware == "" && req.Initrd == "" {
		return errors.New("initrd must not be empty for direct boot")
	}
	if req.Firmware != "" && (req.Kernel != "" || req.Initrd != "") {
		return errors.New("firmware boot cannot be combined with kernel or initrd")
	}
	if req.RunDir == "" {
		return errors.New("run dir must not be empty")
	}
	if req.LogDir == "" {
		return errors.New("log dir must not be empty")
	}
	if req.CPUs < 0 {
		return errors.New("cpus must be greater than zero")
	}
	if req.MemoryBytes < 0 {
		return errors.New("memory bytes must be greater than zero")
	}
	if _, err := normalizeNetworks(req.Network, req.Networks); err != nil {
		return err
	}
	return nil
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate VM ID: %w", err)
	}
	return "kb_" + hex.EncodeToString(raw[:]), nil
}
