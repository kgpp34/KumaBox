package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/lockfile"
	"github.com/kumabox/kumabox/internal/meta"
)

var testBackupStep func(string) error

// Backup atomically replaces destination with a verified, single-file SQLite
// snapshot. A failed run removes only its temporary file and leaves any
// previously published backup intact.
func Backup(ctx context.Context, sourcePath, destinationPath string) (err error) {
	if sourcePath == "" || destinationPath == "" {
		return fmt.Errorf("sqlite backup source and destination are required: %w", meta.ErrScope)
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve sqlite backup source: %w", err)
	}
	destinationPath, err = filepath.Abs(destinationPath)
	if err != nil {
		return fmt.Errorf("resolve sqlite backup destination: %w", err)
	}
	if sourcePath == destinationPath {
		return fmt.Errorf("sqlite backup destination must differ from source: %w", meta.ErrScope)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("stat sqlite backup source: %w", err)
	}
	if err := RefuseConversion(sourcePath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return fmt.Errorf("create sqlite backup directory: %w", err)
	}
	lockKey := filepath.Base(destinationPath) + ".backup"
	lock, err := lockfile.New(filepath.Dir(destinationPath)).Acquire(ctx, lockKey)
	if err != nil {
		return fmt.Errorf("lock sqlite backup destination: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	temporary, err := os.CreateTemp(filepath.Dir(destinationPath), ".kumabox-backup-*.db")
	if err != nil {
		return fmt.Errorf("reserve sqlite backup temporary path: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close sqlite backup temporary file: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("prepare sqlite backup temporary path: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := vacuumInto(ctx, sourcePath, temporaryPath); err != nil {
		return err
	}
	if err := backupStep("vacuumed"); err != nil {
		return err
	}
	if err := verifyDatabaseFile(ctx, temporaryPath); err != nil {
		return fmt.Errorf("verify sqlite backup: %w", err)
	}
	if err := backupStep("verified"); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return fmt.Errorf("set sqlite backup permissions: %w", err)
	}
	if err := syncFile(temporaryPath); err != nil {
		return err
	}
	if err := backupStep("synced"); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return fmt.Errorf("publish sqlite backup: %w", err)
	}
	if err := backupStep("renamed"); err != nil {
		return err
	}
	return syncParent(filepath.Dir(destinationPath))
}

func vacuumInto(ctx context.Context, sourcePath, destinationPath string) (err error) {
	db, err := openDatabase(sourcePath, "FULL", true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := verifyDatabaseIdentity(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destinationPath); err != nil {
		return fmt.Errorf("create sqlite backup: %w", mapError(err))
	}
	return nil
}

func verifyDatabaseFile(ctx context.Context, path string) (err error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(ON)&_pragma=trusted_schema(OFF)")
	if err != nil {
		return fmt.Errorf("open sqlite database for verification: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := verifyDatabaseIdentity(ctx, db); err != nil {
		return err
	}
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return mapError(err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check returned %q: %w", result, meta.ErrCorrupt)
	}
	return nil
}

func verifyDatabaseIdentity(ctx context.Context, db *sql.DB) error {
	var applicationID, schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return mapError(err)
	}
	if applicationID != databaseApplicationID {
		return fmt.Errorf("sqlite application id %d is not KumaBox: %w", applicationID, meta.ErrCorrupt)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return mapError(err)
	}
	if schemaVersion != databaseSchemaVersion {
		return fmt.Errorf("unsupported sqlite schema version %d: %w", schemaVersion, meta.ErrCorrupt)
	}
	var namespaces, invalidStates int
	query := "SELECT count(*), coalesce(sum(CASE WHEN state IN ('initialized', 'converted') THEN 0 ELSE 1 END), 0) FROM " + metadataStateTable
	if err := db.QueryRowContext(ctx, query).Scan(&namespaces, &invalidStates); err != nil {
		return fmt.Errorf("read sqlite metadata namespace state: %w", mapError(err))
	}
	if namespaces == 0 || invalidStates != 0 {
		return fmt.Errorf("sqlite metadata namespace state is incomplete: %w", meta.ErrCorrupt)
	}
	return nil
}

func syncFile(path string) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sqlite backup for sync: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync sqlite backup: %w", err)
	}
	return nil
}

func syncParent(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sqlite backup directory: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync sqlite backup directory: %w", err)
	}
	return nil
}

func backupStep(step string) error {
	if testBackupStep == nil {
		return nil
	}
	return testBackupStep(step)
}
