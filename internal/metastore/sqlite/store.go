// Package sqlite implements the metadata transaction boundary with SQLite.
// Tables contain only an id and an encoded record; typed object handling stays
// in metastore.Collection, just as it does for the JSON engine.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/metastore"
	_ "modernc.org/sqlite"
)

const (
	databaseApplicationID = 0x4b4d4231 // "KMB1"
	databaseSchemaVersion = 1
	metadataStateTable    = "_kumabox_meta_state"
	// ConversionManifestName marks an unfinished offline backend switch.
	ConversionManifestName = "meta-convert.manifest"
)

// Namespace declares the tables an SQLite metadata file may contain.
type Namespace struct {
	Name   metastore.Namespace
	Tables []metastore.Table
}

// NamespaceStatus describes the durable initialization state of one metadata
// namespace. It is intentionally separate from resource records so startup
// can validate the database before opening resource collections.
type NamespaceStatus struct {
	Namespace     metastore.Namespace
	State         string
	SchemaVersion int
	Records       int
	Source        string
	Digest        string
	UpdatedAt     string
}

// Store is an SQLite-backed MetaEngine.
type Store struct {
	path        string
	durable     *sql.DB
	relaxed     *sql.DB
	readers     *sql.DB
	namespaces  map[metastore.Namespace]map[metastore.Table]struct{}
	mu          sync.Mutex
	subscribers map[*subscription]struct{}
	closed      bool
}

type subscription struct {
	changes chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	stop    sync.Once
}

func (s *subscription) close() {
	s.stop.Do(func() {
		s.cancel()
		<-s.done
		close(s.changes)
	})
}

var _ metastore.MetaEngine = (*Store)(nil)

// Open opens an initialized metadata database. Database creation and schema
// changes belong to Init so a normal command can never mistake a partial or
// unrelated SQLite file for an empty KumaBox store.
func Open(path string, definitions ...Namespace) (*Store, error) {
	if err := RefuseConversion(path); err != nil {
		return nil, err
	}
	return open(path, definitions...)
}

// OpenForRecovery bypasses the conversion guard for the conversion command.
func OpenForRecovery(path string, definitions ...Namespace) (*Store, error) {
	return open(path, definitions...)
}

func open(path string, definitions ...Namespace) (*Store, error) {
	if path == "" || len(definitions) == 0 {
		return nil, fmt.Errorf("SQLite metadata path and namespace definitions are required: %w", metastore.ErrScope)
	}
	namespaces, err := validateDefinitions(definitions)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("sqlite metadata database %s is not initialized: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("stat sqlite metadata database: %w", err)
	}
	store := &Store{path: path, namespaces: namespaces, subscribers: make(map[*subscription]struct{})}
	if store.durable, err = openDatabase(path, "FULL", true); err != nil {
		return nil, err
	}
	if store.relaxed, err = openDatabase(path, "NORMAL", true); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if store.readers, err = openDatabase(path, "FULL", false); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	store.readers.SetMaxOpenConns(max(2, runtime.NumCPU()))
	store.readers.SetMaxIdleConns(max(2, runtime.NumCPU()))
	if err := store.initializeIdentity(); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

// RefuseConversion prevents ordinary commands from using either side of an
// unfinished metadata switch.
func RefuseConversion(path string) error {
	manifest := filepath.Join(filepath.Dir(path), ConversionManifestName)
	if _, err := os.Stat(manifest); err == nil {
		return fmt.Errorf("metadata conversion manifest %s exists; rerun metadata convert", manifest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat metadata conversion manifest: %w", err)
	}
	return nil
}

func (s *Store) View(ctx context.Context, namespaces []metastore.Namespace, fn func(metastore.Reader) error) error {
	if fn == nil {
		return fmt.Errorf("metadata view callback must not be nil: %w", metastore.ErrScope)
	}
	if err := s.checkOpenAndScope(namespaces, ""); err != nil {
		return err
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback() //nolint:errcheck
	return fn(&txReader{tx: tx, allowed: namespaceSet(namespaces), tables: s.tablesFor(namespaces)})
}

func (s *Store) Update(ctx context.Context, scope metastore.Scope, mode metastore.CommitMode, fn func(metastore.Writer) error) error {
	if fn == nil {
		return fmt.Errorf("metadata update callback must not be nil: %w", metastore.ErrScope)
	}
	if mode != metastore.CommitDurable && mode != metastore.CommitRelaxed {
		return fmt.Errorf("unsupported metadata commit mode %d: %w", mode, metastore.ErrDurabilityContract)
	}
	namespaces := append([]metastore.Namespace{scope.Write}, scope.Read...)
	if err := s.checkOpenAndScope(namespaces, scope.Write); err != nil {
		return err
	}
	db := s.durable
	if mode == metastore.CommitRelaxed {
		db = s.relaxed
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	writer := &txWriter{txReader: txReader{tx: tx, allowed: namespaceSet(namespaces), tables: s.tablesFor(namespaces)}, writeNamespace: scope.Write, mode: mode}
	if err := fn(writer); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	s.notify()
	return nil
}

func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	conn, err := s.readers.Conn(watchCtx)
	if err != nil {
		cancel()
		return nil, nil, mapError(err)
	}
	version, err := sqliteDataVersion(watchCtx, conn)
	if err != nil {
		_ = conn.Close()
		cancel()
		return nil, nil, err
	}
	sub := &subscription{changes: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		cancel()
		return nil, nil, metastore.ErrClosed
	}
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()
	go s.watchDataVersion(watchCtx, conn, sub, version)

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers, sub)
			s.mu.Unlock()
			sub.close()
		})
	}
	return sub.changes, release, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	subs := make([]*subscription, 0, len(s.subscribers))
	for sub := range s.subscribers {
		subs = append(subs, sub)
		delete(s.subscribers, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
	var closeErrors []error
	for _, db := range []*sql.DB{s.durable, s.relaxed, s.readers} {
		if db != nil {
			closeErrors = append(closeErrors, db.Close())
		}
	}
	s.durable, s.relaxed, s.readers = nil, nil, nil
	return errors.Join(closeErrors...)
}

// Status returns the initialization state recorded for each declared
// namespace. The state is used by migration and recovery tooling rather than
// by normal resource reads and writes.
func (s *Store) Status(ctx context.Context) (result []NamespaceStatus, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, metastore.ErrClosed
	}
	rows, err := s.readers.QueryContext(ctx, "SELECT namespace, state, schema_version, records, source, digest, updated_at FROM "+metadataStateTable+" ORDER BY namespace")
	if err != nil {
		return nil, mapError(err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = mapError(closeErr)
		}
	}()
	for rows.Next() {
		var status NamespaceStatus
		if err := rows.Scan(&status.Namespace, &status.State, &status.SchemaVersion, &status.Records, &status.Source, &status.Digest, &status.UpdatedAt); err != nil {
			return nil, mapError(err)
		}
		if _, declared := s.namespaces[status.Namespace]; declared {
			result = append(result, status)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	if len(result) != len(s.namespaces) {
		return nil, fmt.Errorf("SQLite metadata namespace state is incomplete: %w", metastore.ErrCorrupt)
	}
	return result, nil
}

// Verify checks SQLite's page and index invariants in addition to KumaBox's
// identity and namespace declarations.
func (s *Store) Verify(ctx context.Context) error {
	if _, err := s.Status(ctx); err != nil {
		return err
	}
	var result string
	if err := s.readers.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return mapError(err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite metadata integrity check returned %q: %w", result, metastore.ErrCorrupt)
	}
	return nil
}

func (s *Store) initializeIdentity() error {
	var applicationID int
	if err := s.readers.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return mapError(err)
	}
	if applicationID != databaseApplicationID {
		return fmt.Errorf("sqlite metadata application id %d is not KumaBox: %w", applicationID, metastore.ErrCorrupt)
	}

	var schemaVersion int
	if err := s.readers.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return mapError(err)
	}
	if schemaVersion != databaseSchemaVersion {
		return fmt.Errorf("unsupported sqlite metadata schema version %d: %w", schemaVersion, metastore.ErrCorrupt)
	}
	_, err := s.Status(context.Background())
	return err
}

func (s *Store) checkOpenAndScope(namespaces []metastore.Namespace, write metastore.Namespace) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return metastore.ErrClosed
	}
	seen := make(map[metastore.Namespace]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			return fmt.Errorf("metadata namespace must not be empty: %w", metastore.ErrScope)
		}
		if _, ok := s.namespaces[namespace]; !ok {
			return fmt.Errorf("metadata namespace %q is not declared: %w", namespace, metastore.ErrScope)
		}
		seen[namespace] = struct{}{}
	}
	if write != "" {
		if _, ok := seen[write]; !ok {
			return fmt.Errorf("write namespace %q is outside scope: %w", write, metastore.ErrScope)
		}
	}
	return nil
}

func (s *Store) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for sub := range s.subscribers {
		select {
		case sub.changes <- struct{}{}:
		default:
		}
	}
}

func (s *Store) watchDataVersion(ctx context.Context, conn *sql.Conn, sub *subscription, previous int64) {
	defer close(sub.done)
	defer func() { _ = conn.Close() }()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := sqliteDataVersion(ctx, conn)
			if err != nil || current == previous {
				continue
			}
			previous = current
			s.mu.Lock()
			if _, ok := s.subscribers[sub]; ok && !s.closed {
				select {
				case sub.changes <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
		}
	}
}

func sqliteDataVersion(ctx context.Context, conn *sql.Conn) (int64, error) {
	var version int64
	if err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, mapError(err)
	}
	return version, nil
}

type txReader struct {
	tx      *sql.Tx
	allowed map[metastore.Namespace]struct{}
	tables  map[metastore.Namespace]map[metastore.Table]struct{}
}

func (r *txReader) GetRaw(ctx context.Context, namespace metastore.Namespace, table metastore.Table, id metastore.RecordID) (json.RawMessage, bool, error) {
	if err := r.checkRead(namespace, table); err != nil {
		return nil, false, err
	}
	var raw []byte
	err := r.tx.QueryRowContext(ctx, "SELECT data FROM "+tableName(namespace, table)+" WHERE id = ?", string(id)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	return append(json.RawMessage(nil), raw...), true, nil
}

func (r *txReader) ScanRaw(ctx context.Context, namespace metastore.Namespace, table metastore.Table, fn func(metastore.RecordID, json.RawMessage) error) (err error) {
	if fn == nil {
		return fmt.Errorf("metadata scan callback must not be nil: %w", metastore.ErrScope)
	}
	if err := r.checkRead(namespace, table); err != nil {
		return err
	}
	rows, err := r.tx.QueryContext(ctx, "SELECT id, data FROM "+tableName(namespace, table)+" ORDER BY id")
	if err != nil {
		return mapError(err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = mapError(closeErr)
		}
	}()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return mapError(err)
		}
		if err := fn(metastore.RecordID(id), append(json.RawMessage(nil), raw...)); err != nil {
			return err
		}
	}
	return mapError(rows.Err())
}

func (r *txReader) checkRead(namespace metastore.Namespace, table metastore.Table) error {
	if _, ok := r.allowed[namespace]; !ok {
		return fmt.Errorf("metadata namespace %q is outside transaction scope: %w", namespace, metastore.ErrScope)
	}
	if _, ok := r.tables[namespace][table]; !ok {
		return fmt.Errorf("metadata table %q/%q is not declared: %w", namespace, table, metastore.ErrScope)
	}
	if table == "" {
		return fmt.Errorf("metadata table must not be empty: %w", metastore.ErrScope)
	}
	return nil
}

type txWriter struct {
	txReader
	writeNamespace metastore.Namespace
	mode           metastore.CommitMode
}

func (w *txWriter) PutRaw(ctx context.Context, namespace metastore.Namespace, table metastore.Table, id metastore.RecordID, raw json.RawMessage) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(ctx, "INSERT INTO "+tableName(namespace, table)+" (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(id), []byte(raw))
	return mapError(err)
}

func (w *txWriter) DeleteRaw(ctx context.Context, namespace metastore.Namespace, table metastore.Table, id metastore.RecordID) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(ctx, "DELETE FROM "+tableName(namespace, table)+" WHERE id = ?", string(id))
	return mapError(err)
}

func (w *txWriter) checkWrite(ctx context.Context, namespace metastore.Namespace, table metastore.Table, id metastore.RecordID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if namespace != w.writeNamespace {
		return fmt.Errorf("cannot write metadata namespace %q from %q transaction: %w", namespace, w.writeNamespace, metastore.ErrScope)
	}
	if table == "" || id == "" {
		return fmt.Errorf("metadata table and id must not be empty: %w", metastore.ErrScope)
	}
	if w.mode != metastore.CommitDurable && w.mode != metastore.CommitRelaxed {
		return fmt.Errorf("unsupported metadata commit mode: %d", w.mode)
	}
	return nil
}

func validateDefinitions(definitions []Namespace) (map[metastore.Namespace]map[metastore.Table]struct{}, error) {
	result := make(map[metastore.Namespace]map[metastore.Table]struct{}, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" || len(definition.Tables) == 0 {
			return nil, fmt.Errorf("metadata namespace %q has incomplete definition: %w", definition.Name, metastore.ErrScope)
		}
		if _, exists := result[definition.Name]; exists {
			return nil, fmt.Errorf("metadata namespace %q declared twice: %w", definition.Name, metastore.ErrScope)
		}
		result[definition.Name] = make(map[metastore.Table]struct{}, len(definition.Tables))
		for _, table := range definition.Tables {
			if table == "" {
				return nil, fmt.Errorf("metadata table must not be empty: %w", metastore.ErrScope)
			}
			result[definition.Name][table] = struct{}{}
		}
	}
	return result, nil
}

func namespaceSet(namespaces []metastore.Namespace) map[metastore.Namespace]struct{} {
	result := make(map[metastore.Namespace]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		result[namespace] = struct{}{}
	}
	return result
}

func (s *Store) tablesFor(namespaces []metastore.Namespace) map[metastore.Namespace]map[metastore.Table]struct{} {
	result := make(map[metastore.Namespace]map[metastore.Table]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		result[namespace] = s.namespaces[namespace]
	}
	return result
}

func tableName(namespace metastore.Namespace, table metastore.Table) string {
	name := rawTableName(namespace, table)
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func rawTableName(namespace metastore.Namespace, table metastore.Table) string {
	return string(namespace) + "__" + string(table)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "busy"), strings.Contains(message, "locked"):
		return fmt.Errorf("%v: %w", err, metastore.ErrBusy)
	case strings.Contains(message, "full"), strings.Contains(message, "no space"):
		return fmt.Errorf("%v: %w", err, metastore.ErrNoSpace)
	default:
		return err
	}
}

func openDatabase(path, synchronous string, writer bool) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)" +
		"&_pragma=trusted_schema(OFF)&_pragma=synchronous(" + synchronous + ")"
	if writer {
		dsn += "&_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite metadata database: %w", mapError(err))
	}
	if writer {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	if err := db.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("ping sqlite metadata database: %w", mapError(err)), db.Close())
	}
	return db, nil
}
