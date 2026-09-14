// Package sqlite implements metadata transactions using SQLite WAL, separate
// reader and writer pools, and a cross-process initialization lock. Module record
// payloads remain opaque; image-specific indexes belong to images/catalog.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	moderncsqlite "modernc.org/sqlite"
	modernclib "modernc.org/sqlite/lib"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/storage"
)

const (
	// applicationID distinguishes KumaBox metadata from unrelated SQLite files.
	applicationID = 0x4B554D41
	// schemaVersion identifies the collection/record schema accepted by this engine.
	schemaVersion = 1
	// initLockName serializes schema initialization across processes in this directory.
	initLockName = "init.lock"
)

// Options bounds SQLite lock waits and the overall write transaction lifetime.
// Both durations must be positive.
type Options struct {
	// BusyTimeout is the per-connection SQLite busy-handler wait.
	BusyTimeout time.Duration
	// RetryLimit bounds writer acquisition, begin retries, and callback execution.
	RetryLimit time.Duration
}

// DefaultOptions uses short individual lock waits within a five-second write budget.
func DefaultOptions() Options {
	return Options{BusyTimeout: 50 * time.Millisecond, RetryLimit: 5 * time.Second}
}

// Store is the SQLite implementation of metadata.Store.
type Store struct {
	// readers permits concurrent read snapshots against the WAL database.
	readers *sql.DB
	// writer has one connection and begins immediate transactions before callbacks.
	writer *sql.DB
	// collections is the immutable allowlist shared by transaction handles.
	collections map[metadata.Collection]struct{}
	// retryLimit becomes each Update call's context deadline.
	retryLimit time.Duration
}

var _ metadata.Store = (*Store)(nil)

// Open validates paths and declarations, initializes an empty database under a
// transient file lock, and verifies database identity and existing collections.
// It rejects incompatible populated databases rather than rewriting their schema.
// The caller owns the returned store and must Close it.
func Open(ctx context.Context, path string, collections []metadata.Collection, options Options) (*Store, error) {
	if options.BusyTimeout <= 0 || options.RetryLimit <= 0 {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("sqlite timeouts must be positive"))
	}
	declared := make(map[metadata.Collection]struct{}, len(collections))
	for _, collection := range collections {
		parsed, err := metadata.NewCollection(collection.String())
		if err != nil {
			return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
		}
		if _, exists := declared[parsed]; exists {
			return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("collection %s declared twice", parsed))
		}
		declared[parsed] = struct{}{}
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := storage.CheckPath(file); err != nil {
			return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
		}
	}
	if err := storage.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	guard := filelock.NewTransient(filepath.Join(filepath.Dir(path), initLockName))
	if err := guard.Lock(ctx); err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeStoreBusy, err)
	}
	initErr := initialize(ctx, path, collections, options)
	unlockErr := guard.Unlock(context.WithoutCancel(ctx))
	if err := errors.Join(initErr, unlockErr); err != nil {
		return nil, err
	}

	readers, err := sql.Open("sqlite", dsn(path, options, false))
	if err != nil {
		return nil, mapError(err)
	}
	readers.SetMaxOpenConns(max(2, runtime.NumCPU()))
	writer, err := sql.Open("sqlite", dsn(path, options, true))
	if err != nil {
		return nil, errors.Join(mapError(err), readers.Close())
	}
	writer.SetMaxOpenConns(1)
	store := &Store{readers: readers, writer: writer, collections: declared, retryLimit: options.RetryLimit}
	if err := store.verify(ctx); err != nil {
		return nil, errors.Join(err, readers.Close(), writer.Close())
	}
	return store, nil
}

// View runs one callback in a read-only SQL transaction and rolls back on failure.
func (s *Store) View(ctx context.Context, fn func(metadata.Reader) error) error {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mapError(err)
	}
	handle := &transaction{tx: tx, allowed: s.collections, writable: false}
	if err := fn(handle); err != nil {
		return errors.Join(err, rollback(tx))
	}
	return commit(ctx, tx)
}

// Update retries busy transaction acquisition within RetryLimit. An immediate
// transaction obtains SQLite's writer reservation before invoking the callback,
// so user callbacks run at most once and are never replayed for lock contention.
//
//	write deadline -> BEGIN IMMEDIATE -- busy --> jitter and retry
//	                        |
//	                        v
//	                    callback -> success: COMMIT
//	                        |
//	                        +-----> failure: ROLLBACK
func (s *Store) Update(ctx context.Context, fn func(metadata.Writer) error) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.retryLimit)
	defer cancel()
	for {
		tx, err := s.writer.BeginTx(writeCtx, nil)
		if err == nil {
			handle := &transaction{tx: tx, allowed: s.collections, writable: true}
			if err := fn(handle); err != nil {
				return errors.Join(err, rollback(tx))
			}
			return commit(writeCtx, tx)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if writeCtx.Err() != nil {
			return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeStoreBusy, err)
		}
		if !busy(err) {
			return mapError(err)
		}
		pause := time.Duration(rand.Int64N(int64(2 * time.Millisecond))) //nolint:gosec // scheduling jitter is not cryptographic
		select {
		case <-writeCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeStoreBusy, writeCtx.Err())
		case <-time.After(pause):
		}
	}
}

// Close releases both pools and preserves errors from each.
func (s *Store) Close() error { return errors.Join(s.readers.Close(), s.writer.Close()) }

// verify checks database ownership, schema version, and declared collection presence.
func (s *Store) verify(ctx context.Context) error {
	var appID, version int
	if err := s.readers.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return mapError(err)
	}
	if err := s.readers.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return mapError(err)
	}
	if appID != applicationID || version != schemaVersion {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("unexpected metadata identity %#x/version %d", appID, version))
	}
	for collection := range s.collections {
		var present int
		err := s.readers.QueryRowContext(ctx, "SELECT 1 FROM collections WHERE name = ?", collection.String()).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("collection %s is not initialized", collection))
		}
		if err != nil {
			return mapError(err)
		}
	}
	return nil
}

// initialize creates schema only for an unidentified empty database. The caller
// holds the directory initialization lock for this entire operation.
func initialize(ctx context.Context, path string, collections []metadata.Collection, options Options) (returnErr error) {
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", options.BusyTimeout.Milliseconds()))
	query.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Clean(path), RawQuery: query.Encode()}).String())
	if err != nil {
		return mapError(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { returnErr = errors.Join(returnErr, db.Close()) }()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil {
		return mapError(err)
	}
	var appID, version int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return mapError(err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return mapError(err)
	}
	if tables > 0 {
		if appID != applicationID || version != schemaVersion {
			return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("populated database has identity %#x/version %d", appID, version))
		}
		return nil
	}
	if appID != 0 || version != 0 {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("empty database has unexpected identity"))
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, rollback(tx))
		}
	}()
	statements := []string{
		"CREATE TABLE collections (name TEXT NOT NULL PRIMARY KEY)",
		"CREATE TABLE records (collection TEXT NOT NULL, id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(collection, id), FOREIGN KEY(collection) REFERENCES collections(name))",
		fmt.Sprintf("PRAGMA application_id = %d", applicationID),
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion),
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return mapError(err)
		}
	}
	for _, collection := range collections {
		if _, err := tx.ExecContext(ctx, "INSERT INTO collections(name) VALUES (?)", collection.String()); err != nil {
			return mapError(err)
		}
	}
	return commit(ctx, tx)
}

// dsn configures each connection with WAL durability and foreign-key enforcement;
// writer connections additionally reserve the write lock when a transaction begins.
func dsn(path string, options Options, immediate bool) string {
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", options.BusyTimeout.Milliseconds()))
	if immediate {
		query.Add("_txlock", "immediate")
	}
	return (&url.URL{Scheme: "file", Path: filepath.Clean(path), RawQuery: query.Encode()}).String()
}

// mapError translates engine failures into shared policy while retaining causes
// for errors.Is/errors.As and leaving cancellation or unknown errors intact.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrPermission) {
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return err
	}
	switch sqliteErr.Code() & 0xff {
	case modernclib.SQLITE_BUSY, modernclib.SQLITE_LOCKED:
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeStoreBusy, err)
	case modernclib.SQLITE_CONSTRAINT:
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, err)
	case modernclib.SQLITE_CORRUPT, modernclib.SQLITE_NOTADB:
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
	case modernclib.SQLITE_FULL, modernclib.SQLITE_IOERR, modernclib.SQLITE_CANTOPEN, modernclib.SQLITE_READONLY:
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	default:
		return err
	}
}

// busy recognizes both database contention and table/schema lock contention.
func busy(err error) bool {
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == modernclib.SQLITE_BUSY || code == modernclib.SQLITE_LOCKED
}

// commit handles the database/sql race where cancellation automatically rolls a
// transaction back before Commit observes its context.
// Preserve the cancellation cause without reporting cancellation after a successful commit.
func commit(ctx context.Context, tx *sql.Tx) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, rollback(tx))
	}
	err := tx.Commit()
	if errors.Is(err, sql.ErrTxDone) {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
	}
	return mapError(err)
}

// rollback treats an already completed transaction as successfully cleaned up.
func rollback(tx *sql.Tx) error {
	err := tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return mapError(err)
}
