package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/metastore"
)

// Convert copies declared metadata from another engine into SQLite and marks
// each namespace converted only after all records have been committed.
// Keeping the source untouched makes retry and rollback operationally safe.
func Convert(ctx context.Context, source metastore.MetaEngine, destination *Store, sourceName string, tables []metastore.TableSet) (metastore.TransferReport, error) {
	if sourceName == "" {
		return metastore.TransferReport{}, fmt.Errorf("metadata conversion source must not be empty: %w", metastore.ErrScope)
	}
	if destination == nil {
		return metastore.TransferReport{}, fmt.Errorf("metadata conversion destination must not be nil: %w", metastore.ErrScope)
	}
	report, err := metastore.TransferWithReport(ctx, source, destination, tables)
	if err != nil {
		return metastore.TransferReport{}, err
	}
	if err := destination.markConverted(ctx, sourceName, report); err != nil {
		return metastore.TransferReport{}, err
	}
	return report, nil
}

func (s *Store) markConverted(ctx context.Context, sourceName string, report metastore.TransferReport) error {
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
			return fmt.Errorf("metadata conversion namespace %q is not declared: %w", namespace, metastore.ErrScope)
		}
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	s.notify()
	return nil
}

// MarkConverted records the verified source identity for one namespace.
func (s *Store) MarkConverted(ctx context.Context, namespace metastore.Namespace, sourceName, digest string, records int) error {
	report := metastore.TransferReport{
		Records: map[metastore.Namespace]int{namespace: records},
		Digest:  digest,
	}
	return s.markConverted(ctx, sourceName, report)
}

// Checkpoint folds committed WAL pages into the main database before the file
// is retired or moved.
func Checkpoint(ctx context.Context, path string) (err error) {
	db, err := openDatabase(path, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return mapError(err)
}
