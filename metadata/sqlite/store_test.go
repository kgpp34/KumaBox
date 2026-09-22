package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/metadata"
)

func TestStoreCommitRollbackAndDetachedReads(t *testing.T) {
	collection := metadata.Collection("records")
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "meta.db"), []metadata.Collection{collection}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := store.Update(t.Context(), func(writer metadata.Writer) error {
		return writer.Put(t.Context(), collection, "one", []byte("value"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	rollback := errors.New("rollback")
	if err := store.Update(t.Context(), func(writer metadata.Writer) error {
		if err := writer.Put(t.Context(), collection, "two", []byte("discard")); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rollback error = %v", err)
	}
	if err := store.View(t.Context(), func(reader metadata.Reader) error {
		value, ok, err := reader.Get(t.Context(), collection, "one")
		if err != nil || !ok || string(value) != "value" {
			t.Fatalf("Get one = %q, %v, %v", value, ok, err)
		}
		value[0] = 'X'
		_, ok, err = reader.Get(t.Context(), collection, "two")
		if err != nil || ok {
			t.Fatalf("rolled back record exists: %v, %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestStoreSerializesConcurrentWriters(t *testing.T) {
	collection := metadata.Collection("records")
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "meta.db"), []metadata.Collection{collection}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	var wait sync.WaitGroup
	for index := range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			id := string(rune('a' + index))
			if err := store.Update(context.Background(), func(writer metadata.Writer) error {
				return writer.Put(context.Background(), collection, id, []byte(id))
			}); err != nil {
				t.Errorf("Update %s: %v", id, err)
			}
		}()
	}
	wait.Wait()
	count := 0
	if err := store.View(t.Context(), func(reader metadata.Reader) error {
		return reader.Scan(t.Context(), collection, func(string, []byte) error {
			count++
			return nil
		})
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	if count != 12 {
		t.Fatalf("record count = %d, want 12", count)
	}
}

func TestStoreRejectsForeignDatabaseWithoutChangingJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := db.Exec("CREATE TABLE foreign_data (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(t.Context(), path, []metadata.Collection{"records"}, DefaultOptions()); err == nil {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("adopted foreign database")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeArtifactCorrupt {
		t.Fatalf("identity error = %v", err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("modified foreign database journal = %s", mode)
	}
}

func TestStoreMigratesVersionOneAndPreservesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	legacy := metadata.Collection("images")
	added := metadata.Collection("sandboxes")
	writeVersionOneDatabase(t, path, "CREATE TABLE collections (name TEXT NOT NULL PRIMARY KEY)")

	store, err := Open(t.Context(), path, []metadata.Collection{legacy, added}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open migrated database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.View(t.Context(), func(reader metadata.Reader) error {
		value, exists, err := reader.Get(t.Context(), legacy, "legacy")
		if err != nil {
			return err
		}
		if !exists || string(value) != "keep" {
			return fmt.Errorf("legacy record = %q, %v", value, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(t.Context(), func(writer metadata.Writer) error {
		return writer.Put(t.Context(), added, "new", []byte("sandbox"))
	}); err != nil {
		t.Fatalf("write added collection: %v", err)
	}
	var version int
	if err := store.readers.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
}

func TestStoreMigratesVersionTwoAndPreservesSandboxRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"CREATE TABLE collections (name TEXT NOT NULL PRIMARY KEY)",
		"CREATE TABLE records (collection TEXT NOT NULL, id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(collection, id), FOREIGN KEY(collection) REFERENCES collections(name))",
		fmt.Sprintf("PRAGMA application_id = %d", applicationID),
		"PRAGMA user_version = 2",
		"INSERT INTO collections(name) VALUES ('sandboxes')",
		"INSERT INTO records(collection,id,data) VALUES ('sandboxes','sandbox-id',x'6b656570')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(t.Context(), path, []metadata.Collection{"sandboxes", "network_records"}, DefaultOptions())
	if err != nil {
		t.Fatalf("Open migrated v2 database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.View(t.Context(), func(reader metadata.Reader) error {
		value, exists, err := reader.Get(t.Context(), "sandboxes", "sandbox-id")
		if err != nil {
			return err
		}
		if !exists || string(value) != "keep" {
			return fmt.Errorf("sandbox record = %q, %t", value, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(t.Context(), func(writer metadata.Writer) error {
		return writer.Put(t.Context(), "network_records", "network-id", []byte("network"))
	}); err != nil {
		t.Fatalf("write migrated network collection: %v", err)
	}
}

func TestStoreMigrationFailureRollsBackVersionAndCollections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	writeVersionOneDatabase(t, path, "CREATE TABLE collections (name TEXT NOT NULL PRIMARY KEY CHECK(name <> 'sandboxes'))")

	if store, err := Open(t.Context(), path, []metadata.Collection{"images", "sandboxes"}, DefaultOptions()); err == nil {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("migration unexpectedly succeeded")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != firstSchemaVersion {
		t.Fatalf("schema version after rollback = %d, want %d", version, firstSchemaVersion)
	}
	var added int
	if err := db.QueryRow("SELECT count(*) FROM collections WHERE name = 'sandboxes'").Scan(&added); err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatal("failed migration published sandbox collection")
	}
	var value []byte
	if err := db.QueryRow("SELECT data FROM records WHERE collection = 'images' AND id = 'legacy'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if string(value) != "keep" {
		t.Fatalf("legacy record after rollback = %q", value)
	}
}

// writeVersionOneDatabase creates the exact generic table shape used before
// sandbox collections existed and leaves one image record as migration evidence.
func writeVersionOneDatabase(t *testing.T, path, collectionsDDL string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		collectionsDDL,
		"CREATE TABLE records (collection TEXT NOT NULL, id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(collection, id), FOREIGN KEY(collection) REFERENCES collections(name))",
		fmt.Sprintf("PRAGMA application_id = %d", applicationID),
		fmt.Sprintf("PRAGMA user_version = %d", firstSchemaVersion),
		"INSERT INTO collections(name) VALUES ('images')",
		"INSERT INTO records(collection,id,data) VALUES ('images','legacy',x'6b656570')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreBusyIsBoundedAcrossProcessesAndWithinPool(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meta.db")
			options := Options{BusyTimeout: 5 * time.Millisecond, RetryLimit: 40 * time.Millisecond}
			first, err := Open(t.Context(), path, []metadata.Collection{"records"}, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := first.Close(); err != nil {
					t.Error(err)
				}
			})
			second := first
			if !shared {
				second, err = Open(t.Context(), path, []metadata.Collection{"records"}, options)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := second.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			// Keep the first transaction alive longer than the second store's retry bound.
			first.retryLimit = time.Second
			go func() {
				done <- first.Update(t.Context(), func(metadata.Writer) error { close(ready); <-release; return nil })
			}()
			<-ready
			// For the pool case restore the caller bound while the first transaction retains its own context.
			if shared {
				first.retryLimit = options.RetryLimit
			}
			start := time.Now()
			err = second.Update(t.Context(), func(metadata.Writer) error { return nil })
			close(release)
			if firstErr := <-done; firstErr != nil {
				t.Fatal(firstErr)
			}
			if code, _ := errdefs.CodeOf(err); code != errdefs.CodeStoreBusy {
				t.Fatalf("busy error = %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("busy wait exceeded bound")
			}
		})
	}
}

func TestStorePreservesCancellationAfterAutomaticRollback(t *testing.T) {
	collection := metadata.Collection("records")
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "meta.db"), []metadata.Collection{collection}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = store.Update(ctx, func(writer metadata.Writer) error {
		if err := writer.Put(ctx, collection, "canceled", []byte("discard")); err != nil {
			return err
		}
		cancel()
		// The writer pool has one connection. A second writer can proceed only
		// after database/sql automatically rolls back the canceled transaction.
		return store.Update(t.Context(), func(next metadata.Writer) error {
			return next.Put(t.Context(), collection, "committed", []byte("keep"))
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	if err := store.View(t.Context(), func(reader metadata.Reader) error {
		_, exists, err := reader.Get(t.Context(), collection, "canceled")
		if err != nil {
			return err
		}
		if exists {
			return errors.New("canceled transaction became visible")
		}
		value, exists, err := reader.Get(t.Context(), collection, "committed")
		if err != nil {
			return err
		}
		if !exists || string(value) != "keep" {
			return errors.New("subsequent writer did not commit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
