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
	"github.com/kumabox/kumabox/metadata/metadatatest"
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

func TestStoreContract(t *testing.T) {
	metadatatest.Run(t, func(t *testing.T, collections []metadata.Collection) metadata.Store {
		store, err := Open(t.Context(), filepath.Join(t.TempDir(), "meta.db"), collections, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store
	})
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
