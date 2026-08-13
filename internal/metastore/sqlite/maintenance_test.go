package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/metastore"
)

func TestBackupPublishesVerifiedCurrentState(t *testing.T) {
	definition := Namespace{Name: "vms", Tables: []metastore.Table{"records"}}
	sourcePath := filepath.Join(t.TempDir(), "metastore.db")
	destinationPath := filepath.Join(t.TempDir(), "backup.db")
	if err := Init(t.Context(), sourcePath, definition); err != nil {
		t.Fatal(err)
	}
	store, err := Open(sourcePath, definition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close source store: %v", err)
		}
	})
	writeBackupRecord(t, store, "before")

	if err := Backup(t.Context(), sourcePath, destinationPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions = %o", info.Mode().Perm())
	}
	if got := readBackupRecord(t, destinationPath, definition); got != "before" {
		t.Fatalf("backup record = %q", got)
	}
	writeBackupRecord(t, store, "after")
	if err := Backup(t.Context(), sourcePath, destinationPath); err != nil {
		t.Fatal(err)
	}
	if got := readBackupRecord(t, destinationPath, definition); got != "after" {
		t.Fatalf("replaced backup record = %q", got)
	}
}

func TestBackupFailurePreservesPublishedBackup(t *testing.T) {
	definition := Namespace{Name: "vms", Tables: []metastore.Table{"records"}}
	sourcePath := filepath.Join(t.TempDir(), "metastore.db")
	destinationPath := filepath.Join(t.TempDir(), "backup.db")
	if err := Init(t.Context(), sourcePath, definition); err != nil {
		t.Fatal(err)
	}
	store, err := Open(sourcePath, definition)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close source store: %v", err)
		}
	})
	writeBackupRecord(t, store, "published")
	if err := Backup(t.Context(), sourcePath, destinationPath); err != nil {
		t.Fatal(err)
	}
	writeBackupRecord(t, store, "unpublished")

	injected := errors.New("injected backup failure")
	ctx := fault.WithInjector(t.Context(), fault.InjectorFunc(func(point fault.Point) error {
		if point == fault.MetadataBackupBeforeSwap {
			return injected
		}
		return nil
	}))
	err = Backup(ctx, sourcePath, destinationPath)
	if !errors.Is(err, injected) {
		t.Fatalf("backup error = %v", err)
	}
	if got := readBackupRecord(t, destinationPath, definition); got != "published" {
		t.Fatalf("backup changed after failed publish: %q", got)
	}
}

func TestBackupRejectsSourceAsDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metastore.db")
	definition := Namespace{Name: "vms", Tables: []metastore.Table{"records"}}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	if err := Backup(t.Context(), path, path); !errors.Is(err, metastore.ErrScope) {
		t.Fatalf("same-path backup error = %v", err)
	}
}

func writeBackupRecord(t *testing.T, store *Store, name string) {
	t.Helper()
	collection := metastore.NewCollection[struct {
		Name string `json:"name"`
	}]("vms", "records")
	record := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := store.Update(t.Context(), metastore.Scope{Write: "vms"}, metastore.CommitDurable, func(writer metastore.Writer) error {
		return collection.Upsert(t.Context(), writer, "vm-1", &record)
	}); err != nil {
		t.Fatal(err)
	}
}

func readBackupRecord(t *testing.T, path string, definition Namespace) string {
	t.Helper()
	store, err := Open(path, definition)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close backup store: %v", err)
		}
	}()
	collection := metastore.NewCollection[struct {
		Name string `json:"name"`
	}]("vms", "records")
	var name string
	if err := store.View(t.Context(), []metastore.Namespace{"vms"}, func(reader metastore.Reader) error {
		record, err := collection.Get(t.Context(), reader, "vm-1")
		if err == nil {
			name = record.Name
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return name
}
