package meta

import (
	"context"
	"encoding/json"
	"fmt"
)

// TableSet declares the records copied for one metadata namespace.
//
// The declaration is explicit so a migration cannot accidentally copy
// implementation tables that belong to another resource.
type TableSet struct {
	Namespace Namespace
	Tables    []Table
}

// Transfer copies records from one metadata engine to another.
//
// Existing records in the destination are replaced. Records that exist only
// in the destination are retained; deletion is deliberately a separate
// operation so an interrupted migration never erases unrelated state.
// Encoded records stay inside this package boundary. Callers migrate typed
// data by declaring the same tables they use with meta.Collection.
func Transfer(ctx context.Context, source, destination MetaEngine, tables []TableSet) error {
	if source == nil || destination == nil {
		return fmt.Errorf("metadata transfer engines must not be nil: %w", ErrScope)
	}
	if len(tables) == 0 {
		return fmt.Errorf("metadata transfer requires at least one table set: %w", ErrScope)
	}

	for _, tableSet := range tables {
		if err := transferNamespace(ctx, source, destination, tableSet); err != nil {
			return err
		}
	}
	return nil
}

type transferRecord struct {
	table Table
	id    RecordID
	raw   json.RawMessage
}

func transferNamespace(ctx context.Context, source, destination MetaEngine, tableSet TableSet) error {
	if tableSet.Namespace == "" || len(tableSet.Tables) == 0 {
		return fmt.Errorf("metadata transfer table set is incomplete: %w", ErrScope)
	}
	seen := make(map[Table]struct{}, len(tableSet.Tables))
	for _, table := range tableSet.Tables {
		if table == "" {
			return fmt.Errorf("metadata transfer table must not be empty: %w", ErrScope)
		}
		if _, exists := seen[table]; exists {
			return fmt.Errorf("metadata transfer table %q is duplicated: %w", table, ErrScope)
		}
		seen[table] = struct{}{}
	}

	records := make([]transferRecord, 0)
	if err := source.View(ctx, []Namespace{tableSet.Namespace}, func(reader Reader) error {
		for _, table := range tableSet.Tables {
			if err := reader.ScanRaw(ctx, tableSet.Namespace, table, func(id RecordID, raw json.RawMessage) error {
				if id == "" || !json.Valid(raw) {
					return fmt.Errorf("metadata transfer found invalid record %s/%s/%s: %w", tableSet.Namespace, table, id, ErrCorrupt)
				}
				records = append(records, transferRecord{
					table: table,
					id:    id,
					raw:   append(json.RawMessage(nil), raw...),
				})
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read metadata namespace %s: %w", tableSet.Namespace, err)
	}

	if err := destination.Update(ctx, Scope{Write: tableSet.Namespace}, CommitDurable, func(writer Writer) error {
		for _, record := range records {
			if err := writer.PutRaw(ctx, tableSet.Namespace, record.table, record.id, record.raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("write metadata namespace %s: %w", tableSet.Namespace, err)
	}
	return nil
}
