package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/meta"
)

func TestBackupPublishesVerifiedCurrentState(t *testing.T) {
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	sourcePath := filepath.Join(t.TempDir(), "metadata.db")
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
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	sourcePath := filepath.Join(t.TempDir(), "metadata.db")
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
	path := filepath.Join(t.TempDir(), "metadata.db")
	definition := Namespace{Name: "vms", Tables: []meta.Table{"records"}}
	if err := Init(t.Context(), path, definition); err != nil {
		t.Fatal(err)
	}
	if err := Backup(t.Context(), path, path); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("same-path backup error = %v", err)
	}
}

func writeBackupRecord(t *testing.T, store *Store, name string) {
	t.Helper()
	collection := meta.NewCollection[struct {
		Name string `json:"name"`
	}]("vms", "records")
	record := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := store.Update(t.Context(), meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
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
	collection := meta.NewCollection[struct {
		Name string `json:"name"`
	}]("vms", "records")
	var name string
	if err := store.View(t.Context(), []meta.Namespace{"vms"}, func(reader meta.Reader) error {
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
