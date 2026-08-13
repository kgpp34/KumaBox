package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// Store owns the snapshot index, payload directories, staging, and leases.
type Store struct {
	dataRoot string
	rootDir  string
	engine   meta.MetaEngine
	leaser   *leaser
	vmReader interface {
		List() ([]*vmstore.VMRecord, error)
	}
}

var snapshotIndexCollection = meta.NewCollection[snapshotIndex]("snapshots", snapshotIndexTable)

// NewStore creates a snapshot store under rootDir.
func NewStore(rootDir string) *Store {
	engine := mustOpenSnapshotEngine(JSONNamespace(rootDir))
	return NewStoreWithEngine(rootDir, engine)
}

// JSONNamespace describes the snapshot index used by the JSON metadata backend.
func JSONNamespace(rootDir string) metajson.Namespace {
	dir := filepath.Join(rootDir, "snapshot")
	return metajson.Namespace{
		Name:     "snapshots",
		FilePath: filepath.Join(dir, "index.json"),
		LockPath: filepath.Join(dir, "index.lock"),
		Codec:    indexCodec{},
	}
}

// NewStoreWithVMReader creates the default JSON snapshot store with an
// injected read-only VM dependency.
func NewStoreWithVMReader(rootDir string, vmReader interface {
	List() ([]*vmstore.VMRecord, error)
}) *Store {
	store := NewStore(rootDir)
	store.vmReader = vmReader
	return store
}

// NewStoreWithEngine creates a snapshot store with an injected metadata engine.
func NewStoreWithEngine(rootDir string, engine meta.MetaEngine) *Store {
	return NewStoreWithEngineAndVMReader(rootDir, engine, vmstore.New(rootDir))
}

// NewStoreWithEngineAndVMReader creates a snapshot store with an injected
// read-only VM dependency used for dependency checks during deletion.
func NewStoreWithEngineAndVMReader(rootDir string, engine meta.MetaEngine, vmReader interface {
	List() ([]*vmstore.VMRecord, error)
}) *Store {
	dir := filepath.Join(rootDir, "snapshot")
	return &Store{dataRoot: rootDir, rootDir: dir, engine: engine, leaser: newLeaser(filepath.Join(dir, "leases")), vmReader: vmReader}
}

// MetadataEngine exposes the persistence boundary to migration tools.
func (s *Store) MetadataEngine() meta.MetaEngine { return s.engine }

func mustOpenSnapshotEngine(namespace metajson.Namespace) meta.MetaEngine {
	engine, err := metajson.Open(namespace)
	if err != nil {
		panic(fmt.Sprintf("open snapshot metadata engine: %v", err))
	}
	return engine
}

// Build is an exclusive pending snapshot transaction.
type Build struct {
	store       *Store
	record      *Record
	lease       *Lease
	performance *CaptureMetrics
	finished    bool
}

// Record returns a defensive copy of the pending record.
func (b *Build) Record() *Record { return cloneRecord(b.record) }

// SetPerformance records capture timing for the pending snapshot. It is
// published atomically with the ready record by Finalize.
func (b *Build) SetPerformance(metrics CaptureMetrics) error {
	if b == nil || b.finished {
		return errors.New("snapshot build is already finished")
	}
	b.performance = &metrics
	return nil
}

// Reserve creates a pending record and staging directory while holding the
// snapshot's exclusive build lease until Finalize or Abort.
func (s *Store) Reserve(ctx context.Context, name string) (*Build, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	lease, err := s.leaser.acquire(ctx, id, leaseExclusive, true)
	if err != nil {
		return nil, err
	}
	stagingDir := filepath.Join(s.rootDir, "staging", "capture-"+id)
	dataDir := filepath.Join(s.rootDir, id)
	now := time.Now().UTC()
	rec := &Record{
		ID: id, Name: name, State: StatePending, DataDir: dataDir,
		StagingDir: stagingDir, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
	}
	if err := s.update(func(idx *snapshotIndex) error {
		if _, exists := idx.Names[name]; exists {
			return fmt.Errorf("SNAPSHOT_NAME_CONFLICT: %w: %s", ErrNameConflict, name)
		}
		if err := os.MkdirAll(stagingDir, 0o700); err != nil {
			return fmt.Errorf("create snapshot staging directory: %w", err)
		}
		idx.Snapshots[id] = cloneRecord(rec)
		idx.Names[name] = id
		return nil
	}); err != nil {
		_ = os.RemoveAll(stagingDir)
		_ = lease.Release()
		return nil, err
	}
	return &Build{store: s, record: rec, lease: lease}, nil
}

// Finalize atomically publishes staged payload after snapshot.json exists.
func (b *Build) Finalize(sizeBytes int64) (*Record, error) {
	return b.FinalizeContext(context.Background(), sizeBytes)
}

// FinalizeContext publishes staged payload and is safe to retry when the data
// directory rename completed but the metadata transaction did not.
func (b *Build) FinalizeContext(ctx context.Context, sizeBytes int64) (*Record, error) {
	if b == nil || b.finished {
		return nil, errors.New("snapshot build is already finished")
	}
	payloadDir := b.record.StagingDir
	if _, err := os.Stat(payloadDir); errors.Is(err, os.ErrNotExist) {
		payloadDir = b.record.DataDir
	} else if err != nil {
		return nil, fmt.Errorf("stat snapshot staging directory: %w", err)
	}
	manifest := filepath.Join(payloadDir, ManifestFile)
	info, err := os.Stat(manifest)
	if err != nil {
		return nil, fmt.Errorf("validate snapshot manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("snapshot manifest must be a regular file")
	}
	logicalBytes, allocatedBytes, err := payloadUsage(payloadDir)
	if err != nil {
		return nil, fmt.Errorf("measure snapshot payload: %w", err)
	}
	if allocatedBytes == 0 && sizeBytes > 0 {
		allocatedBytes = sizeBytes
	}

	var finalized *Record
	err = b.store.update(func(idx *snapshotIndex) error {
		rec, ok := idx.Snapshots[b.record.ID]
		if !ok || rec.State != StatePending {
			return errors.New("pending snapshot record disappeared before finalize")
		}
		_, dataErr := os.Stat(rec.DataDir)
		_, stagingErr := os.Stat(rec.StagingDir)
		if dataErr == nil && stagingErr == nil {
			return errors.New("snapshot staging and data directories both exist")
		}
		if dataErr != nil && !errors.Is(dataErr, os.ErrNotExist) {
			return fmt.Errorf("stat snapshot data directory: %w", dataErr)
		}
		if stagingErr != nil && !errors.Is(stagingErr, os.ErrNotExist) {
			return fmt.Errorf("stat snapshot staging directory: %w", stagingErr)
		}
		if errors.Is(dataErr, os.ErrNotExist) {
			if errors.Is(stagingErr, os.ErrNotExist) {
				return errors.New("snapshot payload disappeared before finalize")
			}
			if err := fault.Check(ctx, fault.SnapshotBeforePublish); err != nil {
				return err
			}
			if err := os.Rename(rec.StagingDir, rec.DataDir); err != nil {
				return fmt.Errorf("publish snapshot data directory: %w", err)
			}
			if err := fault.Check(ctx, fault.SnapshotAfterRename); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		rec.State = StateReady
		rec.StagingDir = ""
		rec.SizeBytes = sizeBytes
		rec.LogicalBytes = logicalBytes
		rec.AllocatedBytes = allocatedBytes
		if b.performance != nil {
			metrics := *b.performance
			rec.Performance = &metrics
		}
		rec.UpdatedAt = now
		rec.LastAccessedAt = now
		finalized = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.finished = true
	if err := b.lease.Release(); err != nil {
		return nil, fmt.Errorf("release snapshot build lease: %w", err)
	}
	return finalized, nil
}

func payloadUsage(root string) (logical, allocated int64, err error) {
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		logical += info.Size()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			allocated += stat.Blocks * 512
		} else {
			allocated += info.Size()
		}
		return nil
	})
	return logical, allocated, err
}

// Abort rolls back a pending build and removes its staging directory.
func (b *Build) Abort() error {
	if b == nil || b.finished {
		return nil
	}
	err := b.store.update(func(idx *snapshotIndex) error {
		rec, ok := idx.Snapshots[b.record.ID]
		if ok && rec.State == StatePending {
			delete(idx.Snapshots, rec.ID)
			delete(idx.Names, rec.Name)
		}
		return nil
	})
	removeErr := errors.Join(os.RemoveAll(b.record.StagingDir), os.RemoveAll(b.record.DataDir))
	releaseErr := b.lease.Release()
	b.finished = true
	return errors.Join(err, removeErr, releaseErr)
}

// List returns ready snapshots ordered by creation time.
func (s *Store) List() ([]*Record, error) {
	records := make([]*Record, 0)
	err := s.read(func(idx *snapshotIndex) error {
		for _, rec := range idx.Snapshots {
			if rec.State == StateReady {
				records = append(records, cloneRecord(rec))
			}
		}
		return nil
	})
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt.Before(records[j].CreatedAt) })
	return records, err
}

// Scan returns every indexed state for fail-closed GC reconciliation.
func (s *Store) Scan() ([]*Record, error) {
	records := make([]*Record, 0)
	err := s.read(func(idx *snapshotIndex) error {
		for _, rec := range idx.Snapshots {
			records = append(records, cloneRecord(rec))
		}
		return nil
	})
	return records, err
}

// IsLeased reports whether a build, reader, restore, or delete owns id.
func (s *Store) IsLeased(id string) (bool, error) {
	dependent, _, err := s.snapshotDependency(id)
	if err != nil {
		return false, err
	}
	if dependent {
		return true, nil
	}
	lease, err := s.leaser.acquire(context.Background(), id, leaseExclusive, false)
	if errors.Is(err, ErrInUse) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lease.Release()
}

// Inspect resolves a ready snapshot by ID, name, or unambiguous ID prefix.
func (s *Store) Inspect(ref string) (*Record, error) {
	var result *Record
	err := s.read(func(idx *snapshotIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.Snapshots[id]
		if rec.State != StateReady {
			return fmt.Errorf("SNAPSHOT_NOT_FOUND: %w: %s", ErrNotFound, ref)
		}
		result = cloneRecord(rec)
		return nil
	})
	return result, err
}

// AcquireRead holds a shared lease for payload inspect/export/restore.
func (s *Store) AcquireRead(ctx context.Context, ref string) (*Record, *Lease, error) {
	return s.acquireRead(ctx, ref, true)
}

func (s *Store) acquireRead(ctx context.Context, ref string, touch bool) (*Record, *Lease, error) {
	rec, err := s.Inspect(ref)
	if err != nil {
		return nil, nil, err
	}
	lease, err := s.leaser.acquire(ctx, rec.ID, leaseRead, true)
	if err != nil {
		return nil, nil, err
	}
	var current *Record
	if touch {
		err = s.update(func(idx *snapshotIndex) error {
			candidate := idx.Snapshots[rec.ID]
			if candidate == nil || candidate.State != StateReady {
				return fmt.Errorf("SNAPSHOT_NOT_FOUND: %w: %s", ErrNotFound, rec.ID)
			}
			candidate.LastAccessedAt = time.Now().UTC()
			candidate.UpdatedAt = candidate.LastAccessedAt
			current = cloneRecord(candidate)
			return nil
		})
	} else {
		current, err = s.Inspect(rec.ID)
	}
	if err != nil {
		_ = lease.Release()
		return nil, nil, err
	}
	return current, lease, nil
}

// LoadManifest reads a ready manifest while holding a shared payload lease.
func (s *Store) LoadManifest(ctx context.Context, ref string) (*Manifest, error) {
	return s.loadManifest(ctx, ref, true)
}

// PeekManifest validates and reads a manifest without changing its LRU age.
// It is intended for GC and dependency scans, not payload consumers.
func (s *Store) PeekManifest(ctx context.Context, ref string) (*Manifest, error) {
	return s.loadManifest(ctx, ref, false)
}

func (s *Store) loadManifest(ctx context.Context, ref string, touch bool) (*Manifest, error) {
	rec, lease, err := s.acquireRead(ctx, ref, touch)
	if err != nil {
		return nil, err
	}
	defer lease.Release()                                             //nolint:errcheck
	raw, err := os.ReadFile(filepath.Join(rec.DataDir, ManifestFile)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	if err := stdjson.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if (manifest.SchemaVersion != "kumabox.snapshot.v1" && manifest.SchemaVersion != "kumabox.snapshot.v2") || manifest.ID != rec.ID {
		return nil, errors.New("SNAPSHOT_CORRUPT: manifest identity does not match snapshot index")
	}
	return &manifest, nil
}

// Remove deletes an unused ready snapshot and its payload directory.
func (s *Store) Remove(ref string) (*Record, error) {
	rec, err := s.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lease, err := s.leaser.acquire(context.Background(), rec.ID, leaseExclusive, false)
	if err != nil {
		if errors.Is(err, ErrInUse) {
			return nil, fmt.Errorf("SNAPSHOT_IN_USE: %w: %s", ErrInUse, rec.Name)
		}
		return nil, err
	}
	defer lease.Release() //nolint:errcheck
	if dependent, vmName, err := s.snapshotDependency(rec.ID); err != nil {
		return nil, err
	} else if dependent {
		return nil, fmt.Errorf("SNAPSHOT_IN_USE: %w: %s is required by VM %s", ErrInUse, rec.Name, vmName)
	}

	var removing *Record
	err = s.update(func(idx *snapshotIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		current := idx.Snapshots[id]
		if current.State != StateReady {
			return fmt.Errorf("SNAPSHOT_NOT_FOUND: %w: %s", ErrNotFound, ref)
		}
		current.State = StateDeleting
		current.UpdatedAt = time.Now().UTC()
		removing = cloneRecord(current)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(removing.DataDir); err != nil {
		_ = s.update(func(idx *snapshotIndex) error {
			if current := idx.Snapshots[removing.ID]; current != nil && current.State == StateDeleting {
				current.State = StateReady
				current.UpdatedAt = time.Now().UTC()
			}
			return nil
		})
		return nil, fmt.Errorf("remove snapshot data directory: %w", err)
	}
	err = s.update(func(idx *snapshotIndex) error {
		current := idx.Snapshots[removing.ID]
		if current == nil || current.State != StateDeleting {
			return errors.New("snapshot delete transaction lost its index record")
		}
		delete(idx.Snapshots, removing.ID)
		delete(idx.Names, removing.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	removing.State = StateReady
	return removing, nil
}

func (s *Store) snapshotDependency(snapshotID string) (bool, string, error) {
	if s.vmReader == nil {
		return false, "", errors.New("snapshot dependency reader is not configured")
	}
	records, err := s.vmReader.List()
	if err != nil {
		return false, "", fmt.Errorf("inspect snapshot dependencies: %w", err)
	}
	for _, rec := range records {
		if rec.SnapshotDependency != nil && rec.SnapshotDependency.SnapshotID == snapshotID {
			return true, rec.Name, nil
		}
		if rec.Hibernate != nil && rec.Hibernate.SnapshotID == snapshotID {
			return true, rec.Name, nil
		}
	}
	return false, "", nil
}

func (s *Store) read(fn func(*snapshotIndex) error) error {
	return s.withIndex(false, fn)
}

func (s *Store) update(fn func(*snapshotIndex) error) error {
	return s.withIndex(true, fn)
}

func (s *Store) withIndex(write bool, fn func(*snapshotIndex) error) error {
	ctx := context.Background()
	if write {
		return s.engine.Update(ctx, meta.Scope{Write: "snapshots"}, meta.CommitDurable, func(writer meta.Writer) error {
			idx, err := s.readIndex(ctx, writer)
			if err != nil {
				return err
			}
			if err := fn(idx); err != nil {
				return err
			}
			return snapshotIndexCollection.Upsert(ctx, writer, snapshotIndexRecord, idx)
		})
	}
	return s.engine.View(ctx, []meta.Namespace{"snapshots"}, func(reader meta.Reader) error {
		idx, err := s.readIndex(ctx, reader)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func (s *Store) readIndex(ctx context.Context, reader meta.Reader) (*snapshotIndex, error) {
	idx, err := snapshotIndexCollection.Get(ctx, reader, snapshotIndexRecord)
	if errors.Is(err, meta.ErrNotFound) {
		idx = &snapshotIndex{}
	} else if err != nil {
		return nil, fmt.Errorf("read snapshot index: %w", err)
	}
	idx.init()
	return idx, nil
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate snapshot ID: %w", err)
	}
	return "snap_" + hex.EncodeToString(raw[:]), nil
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("snapshot name must not be empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("snapshot name %q is not safe", name)
	}
	return nil
}

func validateID(id string) error {
	if !strings.HasPrefix(id, "snap_") || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("snapshot ID %q is not safe", id)
	}
	return nil
}
