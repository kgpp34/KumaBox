package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/meta"
)

// Init creates a new SQLite metadata database or adds newly declared
// namespaces to an existing compatible KumaBox database. Existing namespaces
// are validated and never rebuilt, so missing tables remain a corruption error
// rather than being mistaken for an upgrade.
func Init(ctx context.Context, path string, definitions ...Namespace) (err error) {
	if err := RefuseConversion(path); err != nil {
		return err
	}
	return initStore(ctx, path, definitions...)
}

// InitForRecovery creates a conversion target while its manifest is present.
func InitForRecovery(ctx context.Context, path string, definitions ...Namespace) error {
	return initStore(ctx, path, definitions...)
}

func initStore(ctx context.Context, path string, definitions ...Namespace) (err error) {
	if path == "" || len(definitions) == 0 {
		return fmt.Errorf("sqlite metadata path and namespace definitions are required: %w", meta.ErrScope)
	}
	namespaces, err := validateDefinitions(definitions)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create sqlite metadata directory: %w", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		empty, inspectErr := isEmptyDatabase(path)
		if inspectErr != nil {
			return inspectErr
		}
		if !empty {
			return upgradeStore(ctx, path, namespaces)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove incomplete sqlite metadata database: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat sqlite metadata database: %w", statErr)
	}
	db, err := openDatabase(path, "FULL", true)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, db.Close())
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := createSchema(ctx, tx, namespaces); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", databaseApplicationID)); err != nil {
		return mapError(err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", databaseSchemaVersion)); err != nil {
		return mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return mapError(err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close initialized sqlite metadata database: %w", err)
	}
	closed = true
	return syncDatabase(path)
}

func upgradeStore(ctx context.Context, path string, namespaces map[meta.Namespace]map[meta.Table]struct{}) (err error) {
	db, err := openDatabase(path, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := verifyDatabaseIdentity(ctx, tx); err != nil {
		return err
	}

	added := false
	for namespace, tables := range namespaces {
		exists, err := namespaceStateExists(ctx, tx, namespace)
		if err != nil {
			return err
		}
		if exists {
			if err := verifyNamespaceTables(ctx, tx, namespace, tables); err != nil {
				return err
			}
			continue
		}
		if err := createNamespace(ctx, tx, namespace, tables); err != nil {
			return err
		}
		added = true
	}
	if !added {
		return fmt.Errorf("sqlite metadata database %s already contains every declared namespace: %w", path, meta.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return mapError(err)
	}
	return syncDatabase(path)
}

func namespaceStateExists(ctx context.Context, tx *sql.Tx, namespace meta.Namespace) (bool, error) {
	var schemaVersion int
	err := tx.QueryRowContext(ctx, "SELECT schema_version FROM "+metadataStateTable+" WHERE namespace = ?", namespace).Scan(&schemaVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	if schemaVersion != databaseSchemaVersion {
		return false, fmt.Errorf("metadata namespace %q has unsupported schema version %d: %w", namespace, schemaVersion, meta.ErrCorrupt)
	}
	return true, nil
}

func verifyNamespaceTables(ctx context.Context, tx *sql.Tx, namespace meta.Namespace, tables map[meta.Table]struct{}) error {
	for table := range tables {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", rawTableName(namespace, table)).Scan(&count); err != nil {
			return mapError(err)
		}
		if count != 1 {
			return fmt.Errorf("metadata namespace %q is missing table %q: %w", namespace, table, meta.ErrCorrupt)
		}
	}
	return nil
}

func createNamespace(ctx context.Context, tx *sql.Tx, namespace meta.Namespace, tables map[meta.Table]struct{}) error {
	for table := range tables {
		query := "CREATE TABLE " + tableName(namespace, table) + " (id TEXT PRIMARY KEY NOT NULL, data BLOB NOT NULL)"
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return mapError(err)
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+metadataStateTable+" (namespace, state, schema_version, updated_at) VALUES (?, 'initialized', ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))", namespace, databaseSchemaVersion)
	return mapError(err)
}

func isEmptyDatabase(path string) (empty bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return true, nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("inspect existing sqlite metadata database: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
		return false, fmt.Errorf("inspect existing sqlite metadata schema: %w", mapError(err))
	}
	return tables == 0, nil
}

func createSchema(ctx context.Context, tx *sql.Tx, namespaces map[meta.Namespace]map[meta.Table]struct{}) error {
	if _, err := tx.ExecContext(ctx, "CREATE TABLE "+metadataStateTable+" (namespace TEXT PRIMARY KEY NOT NULL, state TEXT NOT NULL, schema_version INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '', digest TEXT NOT NULL DEFAULT '', records INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)"); err != nil {
		return mapError(err)
	}
	for namespace, tables := range namespaces {
		if err := createNamespace(ctx, tx, namespace, tables); err != nil {
			return err
		}
	}
	return nil
}

func syncDatabase(path string) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sqlite metadata database for sync: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync sqlite metadata database: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open sqlite metadata directory for sync: %w", err)
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	if err := dir.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync sqlite metadata directory: %w", err)
	}
	return nil
}
