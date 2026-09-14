package metadata

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Memory is a snapshotting in-memory Store for business tests and engine contracts.
// It is not a durable metadata engine.
type Memory struct {
	mu         sync.RWMutex
	writeToken chan struct{}
	records    map[Collection]map[string][]byte
	closed     bool
}

func NewMemory(collections []Collection) (*Memory, error) {
	records := make(map[Collection]map[string][]byte)
	for _, collection := range collections {
		if _, err := NewCollection(collection.String()); err != nil {
			return nil, err
		}
		if _, exists := records[collection]; exists {
			return nil, errors.New("duplicate collection")
		}
		records[collection] = make(map[string][]byte)
	}
	return &Memory{writeToken: make(chan struct{}, 1), records: records}, nil
}

func (s *Memory) snapshot(ctx context.Context) (*memoryTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("metadata store is closed")
	}
	records := make(map[Collection]map[string][]byte)
	for collection, entries := range s.records {
		records[collection] = make(map[string][]byte)
		for key, value := range entries {
			records[collection][key] = slices.Clone(value)
		}
	}
	return &memoryTransaction{records: records}, nil
}

func (s *Memory) View(ctx context.Context, fn func(Reader) error) error {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	if err := fn(snapshot); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *Memory) Update(ctx context.Context, fn func(Writer) error) error {
	select {
	case s.writeToken <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writeToken }()
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	if err := fn(snapshot); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return errors.New("metadata store is closed")
	}
	s.records = snapshot.records
	return nil
}
func (s *Memory) Close() error { s.mu.Lock(); defer s.mu.Unlock(); s.closed = true; return nil }

type memoryTransaction struct {
	records map[Collection]map[string][]byte
}

func (t *memoryTransaction) collection(ctx context.Context, collection Collection) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, ok := t.records[collection]
	if !ok {
		return nil, errors.New("undeclared metadata collection")
	}
	return records, nil
}

func (t *memoryTransaction) Get(ctx context.Context, collection Collection, key string) ([]byte, bool, error) {
	records, err := t.collection(ctx, collection)
	if err != nil {
		return nil, false, err
	}
	value, ok := records[key]
	return slices.Clone(value), ok, nil
}

func (t *memoryTransaction) Scan(ctx context.Context, collection Collection, fn func(string, []byte) error) error {
	records, err := t.collection(ctx, collection)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(key, slices.Clone(records[key])); err != nil {
			return err
		}
	}
	return nil
}

func (t *memoryTransaction) Put(ctx context.Context, collection Collection, key string, value []byte) error {
	records, err := t.collection(ctx, collection)
	if err != nil {
		return err
	}
	records[key] = slices.Clone(value)
	return nil
}

func (t *memoryTransaction) Delete(ctx context.Context, collection Collection, key string) error {
	records, err := t.collection(ctx, collection)
	if err != nil {
		return err
	}
	delete(records, key)
	return nil
}
