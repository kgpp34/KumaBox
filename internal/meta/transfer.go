package meta

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
)

// TableSet declares the records copied for one metadata namespace.
//
// The declaration is explicit so a migration cannot accidentally copy
// implementation tables that belong to another resource.
type TableSet struct {
	Namespace Namespace
	Tables    []Table
}

// TransferReport is the durable evidence produced by a metadata conversion.
// Counts are keyed by namespace and include all declared tables in that
// namespace.
type TransferReport struct {
	Records map[Namespace]int
	Digest  string
}

// Transfer copies records from one metadata engine to another.
//
// Existing records in the destination are replaced. Records that exist only
// in the destination are retained; deletion is deliberately a separate
// operation so an interrupted migration never erases unrelated state.
// Encoded records stay inside this package boundary. Callers migrate typed
// data by declaring the same tables they use with meta.Collection.
func Transfer(ctx context.Context, source, destination MetaEngine, tables []TableSet) error {
	_, err := TransferWithReport(ctx, source, destination, tables)
	return err
}

// TransferWithReport copies records and returns a deterministic content
// digest. It is used by backend conversion so a restart can distinguish a
// completed import from a partially copied database.
func TransferWithReport(ctx context.Context, source, destination MetaEngine, tables []TableSet) (TransferReport, error) {
	if source == nil || destination == nil {
		return TransferReport{}, fmt.Errorf("metadata transfer engines must not be nil: %w", ErrScope)
	}
	if len(tables) == 0 {
		return TransferReport{}, fmt.Errorf("metadata transfer requires at least one table set: %w", ErrScope)
	}

	report := TransferReport{Records: make(map[Namespace]int, len(tables))}
	digest := sha256.New()
	for _, tableSet := range tables {
		count, err := transferNamespace(ctx, source, destination, tableSet, digest)
		if err != nil {
			return TransferReport{}, err
		}
		report.Records[tableSet.Namespace] = count
	}
	report.Digest = fmt.Sprintf("sha256:%x", digest.Sum(nil))
	return report, nil
}

type transferRecord struct {
	table Table
	id    RecordID
	raw   json.RawMessage
}

func transferNamespace(ctx context.Context, source, destination MetaEngine, tableSet TableSet, digest hash.Hash) (int, error) {
	if tableSet.Namespace == "" || len(tableSet.Tables) == 0 {
		return 0, fmt.Errorf("metadata transfer table set is incomplete: %w", ErrScope)
	}
	seen := make(map[Table]struct{}, len(tableSet.Tables))
	for _, table := range tableSet.Tables {
		if table == "" {
			return 0, fmt.Errorf("metadata transfer table must not be empty: %w", ErrScope)
		}
		if _, exists := seen[table]; exists {
			return 0, fmt.Errorf("metadata transfer table %q is duplicated: %w", table, ErrScope)
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
				writeDigest(digest, tableSet.Namespace, table, id, raw)
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
		return 0, fmt.Errorf("read metadata namespace %s: %w", tableSet.Namespace, err)
	}

	if err := destination.Update(ctx, Scope{Write: tableSet.Namespace}, CommitDurable, func(writer Writer) error {
		for _, record := range records {
			if err := writer.PutRaw(ctx, tableSet.Namespace, record.table, record.id, record.raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write metadata namespace %s: %w", tableSet.Namespace, err)
	}
	return len(records), nil
}

func writeDigest(digest hash.Hash, namespace Namespace, table Table, id RecordID, raw json.RawMessage) {
	// Length prefixes keep adjacent fields unambiguous (for example, "ab"+"c"
	// cannot collide with "a"+"bc").
	for _, value := range []string{string(namespace), string(table), string(id)} {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = digest.Write([]byte(value))
	}
	_, _ = fmt.Fprintf(digest, "%d:", len(raw))
	_, _ = digest.Write(raw)
}
