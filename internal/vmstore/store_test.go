package vmstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateInspectList(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "p0-store",
		RootDisk: "fixtures/base.qcow2",
		Kernel:   "fixtures/vmlinuz",
		Initrd:   "fixtures/initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" {
		t.Fatal("expected VM ID")
	}
	if rec.State != StateCreated {
		t.Fatalf("state = %s", rec.State)
	}
	if !filepath.IsAbs(rec.RootDisk) {
		t.Fatalf("root disk is not absolute: %s", rec.RootDisk)
	}

	got, err := store.Inspect("p0-store")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID {
		t.Fatalf("inspect ID = %s, want %s", got.ID, rec.ID)
	}

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}

	indexPath := filepath.Join(dir, "data", "backends", BackendCloudHypervisor, "index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	req := CreateRequest{
		Name:     "same",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	}

	if _, err := store.Create(req); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(req); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestResolveByIDPrefix(t *testing.T) {
	idx := &VMIndex{
		VMs: map[string]*VMRecord{
			"kb_abcdef": {ID: "kb_abcdef"},
		},
		Names: map[string]string{},
	}

	id, err := idx.Resolve("kb_abc")
	if err != nil {
		t.Fatal(err)
	}
	if id != "kb_abcdef" {
		t.Fatalf("id = %s", id)
	}
}
