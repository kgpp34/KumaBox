package meta

import (
	"context"
	"encoding/json"
	"fmt"
)

// Collection is the typed record boundary for one metadata table. The engine
// stores encoded bytes, but callers read and write detached Go values.
type Collection[R any] struct {
	namespace Namespace
	table     Table
}

// NewCollection binds a collection to one metadata namespace and table.
func NewCollection[R any](namespace Namespace, table Table) *Collection[R] {
	return &Collection[R]{namespace: namespace, table: table}
}

// Get returns a detached record or ErrNotFound.
func (c *Collection[R]) Get(ctx context.Context, reader Reader, id RecordID) (*R, error) {
	raw, ok, err := reader.GetRaw(ctx, c.namespace, c.table, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s/%s %q: %w", c.namespace, c.table, id, ErrNotFound)
	}
	return c.decode(id, raw)
}

// Insert adds a record and fails if the id already exists.
func (c *Collection[R]) Insert(ctx context.Context, writer Writer, id RecordID, record *R) error {
	if _, ok, err := writer.GetRaw(ctx, c.namespace, c.table, id); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s/%s %q exists: %w", c.namespace, c.table, id, ErrConflict)
	}
	return c.put(ctx, writer, id, record)
}

// Replace overwrites an existing record and fails if it is absent.
func (c *Collection[R]) Replace(ctx context.Context, writer Writer, id RecordID, record *R) error {
	if _, ok, err := writer.GetRaw(ctx, c.namespace, c.table, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s/%s %q: %w", c.namespace, c.table, id, ErrNotFound)
	}
	return c.put(ctx, writer, id, record)
}

// Upsert inserts or replaces a record.
func (c *Collection[R]) Upsert(ctx context.Context, writer Writer, id RecordID, record *R) error {
	return c.put(ctx, writer, id, record)
}

// Delete removes a record. Deleting an absent record is idempotent.
func (c *Collection[R]) Delete(ctx context.Context, writer Writer, id RecordID) error {
	return writer.DeleteRaw(ctx, c.namespace, c.table, id)
}

// Scan yields detached records in the engine's stable order.
func (c *Collection[R]) Scan(ctx context.Context, reader Reader, fn func(RecordID, *R) error) error {
	if fn == nil {
		return fmt.Errorf("metadata collection scan callback must not be nil: %w", ErrScope)
	}
	return reader.ScanRaw(ctx, c.namespace, c.table, func(id RecordID, raw json.RawMessage) error {
		record, err := c.decode(id, raw)
		if err != nil {
			return err
		}
		return fn(id, record)
	})
}

// List returns all records detached from the engine state.
func (c *Collection[R]) List(ctx context.Context, reader Reader) (map[RecordID]*R, error) {
	result := make(map[RecordID]*R)
	if err := c.Scan(ctx, reader, func(id RecordID, record *R) error {
		result[id] = record
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Collection[R]) put(ctx context.Context, writer Writer, id RecordID, record *R) error {
	if record == nil {
		return fmt.Errorf("%s/%s %q: nil record: %w", c.namespace, c.table, id, ErrIO)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode %s/%s %q: %w", c.namespace, c.table, id, err)
	}
	return writer.PutRaw(ctx, c.namespace, c.table, id, raw)
}

func (c *Collection[R]) decode(id RecordID, raw json.RawMessage) (*R, error) {
	record := new(R)
	if err := json.Unmarshal(raw, record); err != nil {
		return nil, fmt.Errorf("decode %s/%s %q: %w", c.namespace, c.table, id, err)
	}
	return record, nil
}
