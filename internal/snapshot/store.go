package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
)

// Store owns the snapshot index, payload directories, staging, and leases.
type Store struct {
	rootDir   string
	indexPath string
	lockPath  string
	leaser    *leaser
}

// NewStore creates a snapshot store under rootDir.
func NewStore(rootDir string) *Store {
	dir := filepath.Join(rootDir, "snapshot")
	return &Store{
		rootDir:   dir,
		indexPath: filepath.Join(dir, "index.json"),
		lockPath:  filepath.Join(dir, "index.lock"),
		leaser:    newLeaser(filepath.Join(dir, "leases")),
	}
}

// Build is an exclusive pending snapshot transaction.
type Build struct {
	store    *Store
	record   *Record
	lease    *Lease
	finished bool
}

// Record returns a defensive copy of the pending record.
func (b *Build) Record() *Record { return cloneRecord(b.record) }

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
	if b == nil || b.finished {
		return nil, errors.New("snapshot build is already finished")
	}
	manifest := filepath.Join(b.record.StagingDir, "snapshot.json")
	info, err := os.Stat(manifest)
	if err != nil {
		return nil, fmt.Errorf("validate snapshot manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("snapshot manifest must be a regular file")
	}

	var finalized *Record
	err = b.store.update(func(idx *snapshotIndex) error {
		rec, ok := idx.Snapshots[b.record.ID]
		if !ok || rec.State != StatePending {
			return errors.New("pending snapshot record disappeared before finalize")
		}
		if _, err := os.Stat(rec.DataDir); err == nil {
			return errors.New("snapshot data directory already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat snapshot data directory: %w", err)
		}
		if err := os.Rename(rec.StagingDir, rec.DataDir); err != nil {
			return fmt.Errorf("publish snapshot data directory: %w", err)
		}
		now := time.Now().UTC()
		rec.State = StateReady
		rec.StagingDir = ""
		rec.SizeBytes = sizeBytes
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
	removeErr := os.RemoveAll(b.record.StagingDir)
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
	rec, err := s.Inspect(ref)
	if err != nil {
		return nil, nil, err
	}
	lease, err := s.leaser.acquire(ctx, rec.ID, leaseRead, true)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.Inspect(rec.ID)
	if err != nil {
		_ = lease.Release()
		return nil, nil, err
	}
	return current, lease, nil
}

// LoadManifest reads a ready manifest while holding a shared payload lease.
func (s *Store) LoadManifest(ctx context.Context, ref string) (*Manifest, error) {
	rec, lease, err := s.AcquireRead(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer lease.Release()                                                //nolint:errcheck
	raw, err := os.ReadFile(filepath.Join(rec.DataDir, "snapshot.json")) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.SchemaVersion != "kumabox.snapshot.v1" || manifest.ID != rec.ID {
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

func (s *Store) read(fn func(*snapshotIndex) error) error {
	return s.withIndex(false, fn)
}

func (s *Store) update(fn func(*snapshotIndex) error) error {
	return s.withIndex(true, fn)
}

func (s *Store) withIndex(write bool, fn func(*snapshotIndex) error) error {
	if err := os.MkdirAll(s.rootDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot store: %w", err)
	}
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open snapshot index lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck
	mode := syscall.LOCK_SH
	if write {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), mode); err != nil {
		return fmt.Errorf("lock snapshot index: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	idx := &snapshotIndex{}
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err == nil {
		if err := json.Unmarshal(raw, idx); err != nil {
			return fmt.Errorf("decode snapshot index: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read snapshot index: %w", err)
	}
	idx.init()
	if err := fn(idx); err != nil {
		return err
	}
	if !write {
		return nil
	}
	if err := fileutil.WriteJSONAtomic(s.indexPath, idx, ".snapshot-index-*.tmp"); err != nil {
		return fmt.Errorf("write snapshot index: %w", err)
	}
	return nil
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
