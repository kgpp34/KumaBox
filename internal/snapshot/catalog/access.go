package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/metadata"
	"github.com/kumabox/kumabox/internal/types"
)

// accessBuffer coalesces advisory usage timestamps by immutable snapshot ID.
// Entries remain visible until their batch commits. Retiring a batch advances
// a generation: an overlapping WAL read retries if it could have missed those
// entries. Recording never holds a mutex across database I/O.
//
//	Touch -> pending max(time) -> one transaction -> retire committed entries
//	             |                                    ^
//	             +---- Resolve / List / GC view -------+
type accessBuffer struct {
	mu         sync.Mutex
	pending    map[types.SnapshotID]time.Time
	limit      int
	generation uint64
	flush      chan struct{}
}

// NewBuffered constructs a catalog with bounded write-behind usage tracking.
// The owner must periodically FlushAccesses and flush again after callers drain,
// before closing the engine. A crash may lose the latest unflushed usage times;
// identities, publication and deletion still commit synchronously.
func NewBuffered(store metadata.Store, capacity int) (*Store, error) {
	if store == nil || capacity <= 0 {
		return nil, errors.New("buffered snapshot catalog requires an engine and positive capacity")
	}
	return &Store{store: store, accesses: &accessBuffer{
		pending: make(map[types.SnapshotID]time.Time), limit: capacity, flush: make(chan struct{}, 1),
	}}, nil
}

// view retries a read when a concurrent flush retired its pending entries. A
// callback may run again and must replace its results rather than append to old
// results. Unlike a reader fence held across SQL, retries respect ctx deadlines.
func (s *Store) view(ctx context.Context, read func(metadata.Reader) error) error {
	if s.accesses == nil {
		return s.store.View(ctx, read)
	}
	buffer := s.accesses
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		buffer.mu.Lock()
		generation := buffer.generation
		buffer.mu.Unlock()
		if err := s.store.View(ctx, read); err != nil {
			return err
		}
		buffer.mu.Lock()
		unchanged := generation == buffer.generation
		buffer.mu.Unlock()
		if unchanged {
			return nil
		}
	}
}

func (s *Store) visibleAccess(record types.Snapshot) types.Snapshot {
	if s.accesses != nil {
		s.accesses.mu.Lock()
		accessed := s.accesses.pending[record.ID]
		s.accesses.mu.Unlock()
		if accessed.After(record.LastAccessedAt) {
			record.LastAccessedAt = accessed
		}
	}
	return record
}

func (s *Store) bufferAccess(ctx context.Context, id types.SnapshotID, accessed time.Time) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.view(ctx, func(reader metadata.Reader) error {
		record, err := load(ctx, reader, id)
		if err != nil {
			return err
		}
		if !record.Ready {
			return notFound(id.String())
		}
		result, err = decodeSnapshot(record)
		result = s.visibleAccess(result)
		return err
	})
	if err == nil && accessed.After(result.LastAccessedAt) {
		var latest time.Time
		latest, err = s.recordAccess(ctx, id, accessed)
		if latest.After(result.LastAccessedAt) {
			result.LastAccessedAt = latest
		}
	}
	return result, errdefs.WithContext(err, errdefs.ContextInfo{
		Operation: "access snapshot", Entity: id.String(), Phase: "record access",
		Action: "retry the snapshot operation",
	})
}

func (s *Store) recordAccess(ctx context.Context, id types.SnapshotID, accessed time.Time) (time.Time, error) {
	buffer := s.accesses
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		buffer.mu.Lock()
		previous, exists := buffer.pending[id]
		if exists || len(buffer.pending) < buffer.limit {
			latest := accessed
			if previous.After(latest) {
				latest = previous
			}
			buffer.pending[id] = latest
			buffer.mu.Unlock()
			return latest, nil
		}
		buffer.mu.Unlock()
		// Saturation applies bounded, cancelable backpressure rather than
		// dropping the only recent-access evidence used to fence GC.
		if err := s.FlushAccesses(ctx); err != nil {
			return time.Time{}, err
		}
	}
}

// FlushAccesses persists all captured usage times in one transaction. Failed
// batches remain pending; concurrent newer touches are retained for the next
// flush. Removed, pending and deleting snapshots are skipped without resurrection.
func (s *Store) FlushAccesses(ctx context.Context) error {
	if s.accesses == nil {
		return nil
	}
	buffer := s.accesses
	select {
	case buffer.flush <- struct{}{}:
		defer func() { <-buffer.flush }()
	case <-ctx.Done():
		return ctx.Err()
	}
	buffer.mu.Lock()
	batch := maps.Clone(buffer.pending)
	buffer.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		for id, accessed := range batch {
			raw, found, err := writer.Get(ctx, CollectionSnapshots, id.String())
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			record, err := decode(raw)
			if err != nil {
				return err
			}
			if record.ID != id.String() {
				return corrupt(errors.New("snapshot record key differs from ID"))
			}
			current := record.LastAccessedAt
			if current.IsZero() {
				current = record.CreatedAt
			}
			if !record.Ready || record.Deleting || !accessed.After(current) {
				continue
			}
			record.LastAccessedAt = accessed
			raw, err = json.Marshal(record)
			if err != nil {
				return err
			}
			if err := writer.Put(ctx, CollectionSnapshots, id.String(), raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errdefs.WithContext(err, errdefs.ContextInfo{
			Operation: "flush snapshot access", Phase: "metadata", Action: "retry the usage flush",
		})
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	for id, accessed := range batch {
		if !buffer.pending[id].After(accessed) {
			delete(buffer.pending, id)
		}
	}
	buffer.generation++
	return nil
}
