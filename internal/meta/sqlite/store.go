// Package sqlite implements the metadata transaction boundary with SQLite.
// Tables contain only an id and an encoded record; typed object handling stays
// in meta.Collection, just as it does for the JSON engine.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kumabox/kumabox/internal/meta"
	_ "modernc.org/sqlite"
)

const (
	databaseApplicationID = 0x4b4d4231 // "KMB1"
	databaseSchemaVersion = 1
	metadataStateTable    = "_kumabox_meta_state"
)

// Namespace declares the tables an SQLite metadata file may contain.
type Namespace struct {
	Name   meta.Namespace
	Tables []meta.Table
}

// NamespaceStatus describes the durable initialization state of one metadata
// namespace. It is intentionally separate from resource records so startup
// can validate the database before opening resource collections.
type NamespaceStatus struct {
	Namespace     meta.Namespace
	State         string
	SchemaVersion int
	Records       int
	Source        string
	Digest        string
	UpdatedAt     string
}

// Store is an SQLite-backed MetaEngine.
type Store struct {
	db          *sql.DB
	namespaces  map[meta.Namespace]map[meta.Table]struct{}
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
	closed      bool
}

var _ meta.MetaEngine = (*Store)(nil)

// Open opens or creates a metadata database and declares its tables.
func Open(path string, definitions ...Namespace) (*Store, error) {
	if path == "" || len(definitions) == 0 {
		return nil, fmt.Errorf("SQLite metadata path and namespace definitions are required: %w", meta.ErrScope)
	}
	namespaces, err := validateDefinitions(definitions)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create SQLite metadata directory: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, namespaces: namespaces, subscribers: make(map[chan struct{}]struct{})}
	if err := store.initializeIdentity(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.createTables(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) View(ctx context.Context, namespaces []meta.Namespace, fn func(meta.Reader) error) error {
	if fn == nil {
		return fmt.Errorf("metadata view callback must not be nil: %w", meta.ErrScope)
	}
	if err := s.checkOpenAndScope(namespaces, ""); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback() //nolint:errcheck
	return fn(&txReader{tx: tx, allowed: namespaceSet(namespaces), tables: s.tablesFor(namespaces)})
}

func (s *Store) Update(ctx context.Context, scope meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	if fn == nil {
		return fmt.Errorf("metadata update callback must not be nil: %w", meta.ErrScope)
	}
	namespaces := append([]meta.Namespace{scope.Write}, scope.Read...)
	if err := s.checkOpenAndScope(namespaces, scope.Write); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, meta.ErrClosed
	}
	ch := make(chan struct{}, 1)
	s.subscribers[ch] = struct{}{}
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
	return ch, release, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
	s.mu.Unlock()
	return s.db.Close()
}

// Status returns the initialization state recorded for each declared
// namespace. The state is used by migration and recovery tooling rather than
// by normal resource reads and writes.
func (s *Store) Status(ctx context.Context) ([]NamespaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, meta.ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, state, schema_version, records, source, digest, updated_at FROM "+metadataStateTable+" ORDER BY namespace")
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var result []NamespaceStatus
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
		return nil, fmt.Errorf("SQLite metadata namespace state is incomplete: %w", meta.ErrCorrupt)
	}
	return result, nil
}

func (s *Store) initializeIdentity() error {
	var applicationID int
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return mapError(err)
	}
	if applicationID == 0 {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA application_id = %d", databaseApplicationID)); err != nil {
			return mapError(err)
		}
	} else if applicationID != databaseApplicationID {
		return fmt.Errorf("SQLite metadata application id %d is not KumaBox: %w", applicationID, meta.ErrCorrupt)
	}

	var schemaVersion int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return mapError(err)
	}
	if schemaVersion == 0 {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", databaseSchemaVersion)); err != nil {
			return mapError(err)
		}
	} else if schemaVersion != databaseSchemaVersion {
		return fmt.Errorf("unsupported SQLite metadata schema version %d: %w", schemaVersion, meta.ErrCorrupt)
	}
	return nil
}

func (s *Store) createTables() error {
	tx, err := s.db.Begin()
	if err != nil {
		return mapError(err)
	}
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS " + metadataStateTable + " (namespace TEXT PRIMARY KEY NOT NULL, state TEXT NOT NULL, schema_version INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '', digest TEXT NOT NULL DEFAULT '', records INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)"); err != nil {
		_ = tx.Rollback()
		return mapError(err)
	}
	for namespace, tables := range s.namespaces {
		for table := range tables {
			query := "CREATE TABLE IF NOT EXISTS " + tableName(namespace, table) + " (id TEXT PRIMARY KEY NOT NULL, data BLOB NOT NULL)"
			if _, err := tx.Exec(query); err != nil {
				_ = tx.Rollback()
				return mapError(err)
			}
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO "+metadataStateTable+" (namespace, state, schema_version, updated_at) VALUES (?, 'initialized', ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))", namespace, databaseSchemaVersion); err != nil {
			_ = tx.Rollback()
			return mapError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	_, err = s.Status(context.Background())
	return err
}

func (s *Store) checkOpenAndScope(namespaces []meta.Namespace, write meta.Namespace) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return meta.ErrClosed
	}
	seen := make(map[meta.Namespace]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			return fmt.Errorf("metadata namespace must not be empty: %w", meta.ErrScope)
		}
		if _, ok := s.namespaces[namespace]; !ok {
			return fmt.Errorf("metadata namespace %q is not declared: %w", namespace, meta.ErrScope)
		}
		seen[namespace] = struct{}{}
	}
	if write != "" {
		if _, ok := seen[write]; !ok {
			return fmt.Errorf("write namespace %q is outside scope: %w", write, meta.ErrScope)
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
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type txReader struct {
	tx      *sql.Tx
	allowed map[meta.Namespace]struct{}
	tables  map[meta.Namespace]map[meta.Table]struct{}
}

func (r *txReader) GetRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) (json.RawMessage, bool, error) {
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

func (r *txReader) ScanRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, fn func(meta.RecordID, json.RawMessage) error) error {
	if fn == nil {
		return fmt.Errorf("metadata scan callback must not be nil: %w", meta.ErrScope)
	}
	if err := r.checkRead(namespace, table); err != nil {
		return err
	}
	rows, err := r.tx.QueryContext(ctx, "SELECT id, data FROM "+tableName(namespace, table)+" ORDER BY id")
	if err != nil {
		return mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return mapError(err)
		}
		if err := fn(meta.RecordID(id), append(json.RawMessage(nil), raw...)); err != nil {
			return err
		}
	}
	return mapError(rows.Err())
}

func (r *txReader) checkRead(namespace meta.Namespace, table meta.Table) error {
	if _, ok := r.allowed[namespace]; !ok {
		return fmt.Errorf("metadata namespace %q is outside transaction scope: %w", namespace, meta.ErrScope)
	}
	if _, ok := r.tables[namespace][table]; !ok {
		return fmt.Errorf("metadata table %q/%q is not declared: %w", namespace, table, meta.ErrScope)
	}
	if table == "" {
		return fmt.Errorf("metadata table must not be empty: %w", meta.ErrScope)
	}
	return nil
}

type txWriter struct {
	txReader
	writeNamespace meta.Namespace
	mode           meta.CommitMode
}

func (w *txWriter) PutRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID, raw json.RawMessage) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(ctx, "INSERT INTO "+tableName(namespace, table)+" (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(id), []byte(raw))
	return mapError(err)
}

func (w *txWriter) DeleteRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(ctx, "DELETE FROM "+tableName(namespace, table)+" WHERE id = ?", string(id))
	return mapError(err)
}

func (w *txWriter) checkWrite(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if namespace != w.writeNamespace {
		return fmt.Errorf("cannot write metadata namespace %q from %q transaction: %w", namespace, w.writeNamespace, meta.ErrScope)
	}
	if table == "" || id == "" {
		return fmt.Errorf("metadata table and id must not be empty: %w", meta.ErrScope)
	}
	if w.mode != meta.CommitDurable && w.mode != meta.CommitRelaxed {
		return fmt.Errorf("unsupported metadata commit mode: %d", w.mode)
	}
	return nil
}

func validateDefinitions(definitions []Namespace) (map[meta.Namespace]map[meta.Table]struct{}, error) {
	result := make(map[meta.Namespace]map[meta.Table]struct{}, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" || len(definition.Tables) == 0 {
			return nil, fmt.Errorf("metadata namespace %q has incomplete definition: %w", definition.Name, meta.ErrScope)
		}
		if _, exists := result[definition.Name]; exists {
			return nil, fmt.Errorf("metadata namespace %q declared twice: %w", definition.Name, meta.ErrScope)
		}
		result[definition.Name] = make(map[meta.Table]struct{}, len(definition.Tables))
		for _, table := range definition.Tables {
			if table == "" {
				return nil, fmt.Errorf("metadata table must not be empty: %w", meta.ErrScope)
			}
			result[definition.Name][table] = struct{}{}
		}
	}
	return result, nil
}

func namespaceSet(namespaces []meta.Namespace) map[meta.Namespace]struct{} {
	result := make(map[meta.Namespace]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		result[namespace] = struct{}{}
	}
	return result
}

func (s *Store) tablesFor(namespaces []meta.Namespace) map[meta.Namespace]map[meta.Table]struct{} {
	result := make(map[meta.Namespace]map[meta.Table]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		result[namespace] = s.namespaces[namespace]
	}
	return result
}

func tableName(namespace meta.Namespace, table meta.Table) string {
	name := string(namespace) + "__" + string(table)
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "busy"), strings.Contains(message, "locked"):
		return fmt.Errorf("%v: %w", err, meta.ErrBusy)
	case strings.Contains(message, "full"), strings.Contains(message, "no space"):
		return fmt.Errorf("%v: %w", err, meta.ErrNoSpace)
	default:
		return err
	}
}
