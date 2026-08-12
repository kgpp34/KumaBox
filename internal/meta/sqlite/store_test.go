package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestStorePersistsTypedCollectionAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	if err := Init(t.Context(), path, Namespace{Name: "vms", Tables: []meta.Table{"records"}}); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	type record struct {
		Name string `json:"name"`
	}
	collection := meta.NewCollection[record]("vms", "records")

	if err := store.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		return collection.Upsert(ctx, writer, "vm-1", &record{Name: "one"})
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("abort")
	if err := store.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		if err := collection.Upsert(ctx, writer, "vm-2", &record{Name: "two"}); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback error = %v", err)
	}

	if err := store.View(ctx, []meta.Namespace{"vms"}, func(reader meta.Reader) error {
		got, err := collection.Get(ctx, reader, "vm-1")
		if err != nil {
			return err
		}
		if got.Name != "one" {
			t.Fatalf("record name = %q", got.Name)
		}
		if _, err := collection.Get(ctx, reader, "vm-2"); !errors.Is(err, meta.ErrNotFound) {
			t.Fatalf("rolled-back record error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if err := store.View(ctx, []meta.Namespace{"vms"}, func(reader meta.Reader) error {
		got, err := collection.Get(ctx, reader, "vm-1")
		if err != nil {
			return err
		}
		if got.Name != "one" {
			t.Fatalf("reopened record name = %q", got.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreEventsObserveAnotherConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path, definition)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	writer, err := Open(path, definition)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	changes, release, err := reader.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := writer.Update(t.Context(), meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(t.Context(), "vms", "records", "external", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not observe external SQLite connection commit")
	}
}

func TestStoreEventsReleaseMayRaceClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, definition)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := store.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); release() }()
	go func() { defer wait.Done(); _ = store.Close() }()
	wait.Wait()
}

func TestStoreEnforcesDeclaredScopeAndCoalescesEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	definitions := []Namespace{
		Namespace{Name: "vms", Tables: []meta.Table{"records"}},
		Namespace{Name: "network", Tables: []meta.Table{"leases"}},
	}
	if err := Init(t.Context(), path, definitions...); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, definitions...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	ctx := context.Background()
	if err := store.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		return writer.PutRaw(ctx, "network", "leases", "ip-1", []byte(`{}`))
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("write scope error = %v", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vms"}, func(reader meta.Reader) error {
		_, _, err := reader.GetRaw(ctx, "network", "leases", "ip-1")
		return err
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("read scope error = %v", err)
	}

	changes, release, err := store.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, id := range []string{"a", "b"} {
		if err := store.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitRelaxed, func(writer meta.Writer) error {
			return writer.PutRaw(ctx, "vms", "records", meta.RecordID(id), []byte(`{}`))
		}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-changes:
	default:
		t.Fatal("expected metadata change event")
	}
	select {
	case <-changes:
		t.Fatal("metadata events should coalesce")
	default:
	}
}

func TestStoreRecordsIdentityAndNamespaceStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	if err := Init(t.Context(), path, Namespace{Name: "vms", Tables: []meta.Table{"records"}}); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].Namespace != "vms" || status[0].State != "initialized" || status[0].SchemaVersion != databaseSchemaVersion {
		t.Fatalf("namespace status = %+v", status)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA application_id = 1234"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}}); !errors.Is(err, meta.ErrCorrupt) {
		t.Fatalf("wrong application id error = %v", err)
	}
}

func TestStoreRejectsUnsupportedSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	if err := Init(t.Context(), path, Namespace{Name: "vms", Tables: []meta.Table{"records"}}); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Namespace{Name: "vms", Tables: []meta.Table{"records"}}); !errors.Is(err, meta.ErrCorrupt) {
		t.Fatalf("wrong schema version error = %v", err)
	}
}

func TestOpenRequiresInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	if _, err := Open(path, definition); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open uninitialized database error = %v", err)
	}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	if err := Init(t.Context(), path, definition); !errors.Is(err, meta.ErrConflict) {
		t.Fatalf("reinitialize database error = %v", err)
	}
}

func TestStoreSupportsConcurrentReadersAndSerializedWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, definition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if store.durable == store.relaxed || store.durable == store.readers {
		t.Fatal("durable, relaxed, and reader handles must be independent")
	}

	const workers = 8
	var wait sync.WaitGroup
	errorsCh := make(chan error, workers)
	for worker := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			id := meta.RecordID(strconv.Itoa(worker))
			if err := store.Update(t.Context(), meta.Scope{Write: "vms"}, meta.CommitRelaxed, func(writer meta.Writer) error {
				return writer.PutRaw(t.Context(), "vms", "records", id, []byte(`{"ok":true}`))
			}); err != nil {
				errorsCh <- err
				return
			}
			if err := store.View(t.Context(), []meta.Namespace{"vms"}, func(reader meta.Reader) error {
				_, found, err := reader.GetRaw(t.Context(), "vms", "records", id)
				if err == nil && !found {
					return errors.New("written record was not found")
				}
				return err
			}); err != nil {
				errorsCh <- err
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
}
