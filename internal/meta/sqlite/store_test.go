package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestStorePersistsTypedCollectionAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
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
	defer store.Close()
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

func TestStoreEnforcesDeclaredScopeAndCoalescesEvents(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "meta.db"),
		Namespace{Name: "vms", Tables: []meta.Table{"records"}},
		Namespace{Name: "network", Tables: []meta.Table{"leases"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
