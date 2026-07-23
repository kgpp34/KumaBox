package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// MemoryEngine is a deterministic engine for contract tests and small
// in-process uses. It is not a persistence backend.
type MemoryEngine struct {
	mu          sync.RWMutex
	data        map[string]map[string]map[string]json.RawMessage
	subscribers map[chan struct{}]struct{}
	closed      bool
}

// NewMemoryEngine creates an engine with an explicit metadata namespace set.
func NewMemoryEngine(namespaces ...string) (*MemoryEngine, error) {
	if len(namespaces) == 0 {
		return nil, fmt.Errorf("memory engine requires at least one namespace: %w", ErrScope)
	}
	seen := make(map[string]struct{}, len(namespaces))
	data := make(map[string]map[string]map[string]json.RawMessage, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			return nil, fmt.Errorf("metadata namespace must not be empty: %w", ErrScope)
		}
		if _, ok := seen[namespace]; ok {
			return nil, fmt.Errorf("metadata namespace %q declared twice: %w", namespace, ErrScope)
		}
		seen[namespace] = struct{}{}
		data[namespace] = make(map[string]map[string]json.RawMessage)
	}
	return &MemoryEngine{
		data:        data,
		subscribers: make(map[chan struct{}]struct{}),
	}, nil
}

func (e *MemoryEngine) View(ctx context.Context, namespaces []Namespace, fn func(Reader) error) error {
	if fn == nil {
		return fmt.Errorf("metadata view callback must not be nil: %w", ErrScope)
	}
	ordered, err := e.resolveScope(namespaces, "")
	if err != nil {
		return err
	}
	if err := contextErr(ctx); err != nil {
		return err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return ErrClosed
	}
	view := cloneNamespaces(e.data, ordered)
	return fn(memoryReader{data: view, allowed: namespaceSet(ordered)})
}

func (e *MemoryEngine) Update(ctx context.Context, scope Scope, _ CommitMode, fn func(Writer) error) error {
	if fn == nil {
		return fmt.Errorf("metadata update callback must not be nil: %w", ErrScope)
	}
	ordered, err := e.resolveScope(append([]Namespace{scope.Write}, scope.Read...), scope.Write)
	if err != nil {
		return err
	}
	if err := contextErr(ctx); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	working := cloneNamespaces(e.data, ordered)
	writer := &memoryWriter{
		memoryReader:   memoryReader{data: working, allowed: namespaceSet(ordered)},
		writeNamespace: scope.Write,
	}
	if err := fn(writer); err != nil {
		return err
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	for namespace, tables := range working {
		e.data[namespace] = tables
	}
	e.notifyLocked()
	return nil
}

func (e *MemoryEngine) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, nil, ErrClosed
	}
	ch := make(chan struct{}, 1)
	e.subscribers[ch] = struct{}{}
	var once sync.Once
	release := func() {
		once.Do(func() {
			e.mu.Lock()
			if _, ok := e.subscribers[ch]; ok {
				delete(e.subscribers, ch)
				close(ch)
			}
			e.mu.Unlock()
		})
	}
	return ch, release, nil
}

func (e *MemoryEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	for ch := range e.subscribers {
		close(ch)
		delete(e.subscribers, ch)
	}
	return nil
}

func (e *MemoryEngine) resolveScope(namespaces []Namespace, write Namespace) ([]string, error) {
	seen := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			return nil, fmt.Errorf("metadata namespace must not be empty: %w", ErrScope)
		}
		if _, ok := e.data[string(namespace)]; !ok {
			return nil, fmt.Errorf("metadata namespace %q is not declared: %w", namespace, ErrScope)
		}
		if _, ok := seen[string(namespace)]; ok {
			continue
		}
		seen[string(namespace)] = struct{}{}
	}
	if write != "" {
		if _, ok := seen[string(write)]; !ok {
			return nil, fmt.Errorf("write namespace %q is outside scope: %w", write, ErrScope)
		}
	}
	ordered := make([]string, 0, len(seen))
	for namespace := range seen {
		ordered = append(ordered, namespace)
	}
	sort.Strings(ordered)
	return ordered, nil
}

func (e *MemoryEngine) notifyLocked() {
	for ch := range e.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type memoryReader struct {
	data    map[string]map[string]map[string]json.RawMessage
	allowed map[string]struct{}
}

func (r memoryReader) GetRaw(ctx context.Context, namespace Namespace, table Table, id RecordID) (json.RawMessage, bool, error) {
	if err := contextErr(ctx); err != nil {
		return nil, false, err
	}
	if err := r.checkRead(namespace); err != nil {
		return nil, false, err
	}
	tableData, ok := r.data[string(namespace)][string(table)]
	if !ok {
		return nil, false, nil
	}
	raw, ok := tableData[string(id)]
	return cloneRaw(raw), ok, nil
}

func (r memoryReader) ScanRaw(ctx context.Context, namespace Namespace, table Table, fn func(RecordID, json.RawMessage) error) error {
	if fn == nil {
		return fmt.Errorf("metadata scan callback must not be nil: %w", ErrScope)
	}
	if err := r.checkRead(namespace); err != nil {
		return err
	}
	tableData := r.data[string(namespace)][string(table)]
	ids := make([]string, 0, len(tableData))
	for id := range tableData {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := fn(RecordID(id), cloneRaw(tableData[id])); err != nil {
			return err
		}
	}
	return nil
}

func (r memoryReader) checkRead(namespace Namespace) error {
	if _, ok := r.allowed[string(namespace)]; !ok {
		return fmt.Errorf("cannot read metadata namespace %q outside transaction scope: %w", namespace, ErrScope)
	}
	return nil
}

type memoryWriter struct {
	memoryReader
	writeNamespace Namespace
}

func (w *memoryWriter) PutRaw(ctx context.Context, namespace Namespace, table Table, id RecordID, raw json.RawMessage) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("metadata value must not be nil: %w", ErrIO)
	}
	if w.data[string(namespace)][string(table)] == nil {
		w.data[string(namespace)][string(table)] = make(map[string]json.RawMessage)
	}
	w.data[string(namespace)][string(table)][string(id)] = cloneRaw(raw)
	return nil
}

func (w *memoryWriter) DeleteRaw(ctx context.Context, namespace Namespace, table Table, id RecordID) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	delete(w.data[string(namespace)][string(table)], string(id))
	return nil
}

func (w *memoryWriter) checkWrite(ctx context.Context, namespace Namespace, table Table, id RecordID) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if namespace != w.writeNamespace {
		return fmt.Errorf("cannot write metadata namespace %q from %q transaction: %w", namespace, w.writeNamespace, ErrScope)
	}
	if table == "" || id == "" {
		return fmt.Errorf("metadata table and id must not be empty: %w", ErrScope)
	}
	return nil
}

func cloneNamespaces(data map[string]map[string]map[string]json.RawMessage, namespaces []string) map[string]map[string]map[string]json.RawMessage {
	clone := make(map[string]map[string]map[string]json.RawMessage, len(namespaces))
	for _, namespace := range namespaces {
		tables := make(map[string]map[string]json.RawMessage)
		for table, records := range data[namespace] {
			copied := make(map[string]json.RawMessage, len(records))
			for id, raw := range records {
				copied[id] = cloneRaw(raw)
			}
			tables[table] = copied
		}
		clone[namespace] = tables
	}
	return clone
}

func namespaceSet(namespaces []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = struct{}{}
	}
	return allowed
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
