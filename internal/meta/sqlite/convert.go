package sqlite

import (
	"context"
	"fmt"

	"github.com/kumabox/kumabox/internal/meta"
)

// Convert copies declared metadata from another engine into SQLite and marks
// each namespace converted only after all records have been committed.
// Keeping the source untouched makes retry and rollback operationally safe.
func Convert(ctx context.Context, source meta.MetaEngine, destination *Store, sourceName string, tables []meta.TableSet) (meta.TransferReport, error) {
	if sourceName == "" {
		return meta.TransferReport{}, fmt.Errorf("metadata conversion source must not be empty: %w", meta.ErrScope)
	}
	if destination == nil {
		return meta.TransferReport{}, fmt.Errorf("metadata conversion destination must not be nil: %w", meta.ErrScope)
	}
	report, err := meta.TransferWithReport(ctx, source, destination, tables)
	if err != nil {
		return meta.TransferReport{}, err
	}
	if err := destination.markConverted(ctx, sourceName, report); err != nil {
		return meta.TransferReport{}, err
	}
	return report, nil
}

func (s *Store) markConverted(ctx context.Context, sourceName string, report meta.TransferReport) error {
	tx, err := s.durable.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	for namespace, count := range report.Records {
		result, execErr := tx.ExecContext(ctx, "UPDATE "+metadataStateTable+" SET state='converted', records=?, source=?, digest=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE namespace=? AND schema_version=?", count, sourceName, report.Digest, namespace, databaseSchemaVersion)
		if execErr != nil {
			_ = tx.Rollback()
			return mapError(execErr)
		}
		updated, execErr := result.RowsAffected()
		if execErr != nil || updated != 1 {
			_ = tx.Rollback()
			if execErr != nil {
				return mapError(execErr)
			}
			return fmt.Errorf("metadata conversion namespace %q is not declared: %w", namespace, meta.ErrScope)
		}
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	s.notify()
	return nil
}
