package imagestore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreCreateListInspectAndResolve(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "data"))

	rec, err := store.Create(CreateRequest{
		Name:   "ubuntu",
		Source: Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: RootDisk{
			Path:   "base.qcow2",
			Format: "qcow2",
		},
		Boot: Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.ID, "img_") {
		t.Fatalf("image id = %s", rec.ID)
	}

	byName, err := store.Inspect("ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != rec.ID || byName.RootDisk.Format != "qcow2" {
		t.Fatalf("inspect by name = %+v", byName)
	}

	byPrefix, err := store.Inspect(rec.ID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if byPrefix.ID != rec.ID {
		t.Fatalf("inspect by prefix = %+v", byPrefix)
	}

	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "ubuntu" {
		t.Fatalf("records = %+v", records)
	}
}

func TestStoreRejectsDuplicateImageName(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "data"))

	if _, err := store.Create(CreateRequest{Name: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateRequest{Name: "ubuntu"}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("expected ErrNameConflict, got %v", err)
	}
}

func TestResolveAmbiguousImagePrefix(t *testing.T) {
	idx := &imageIndex{
		Images: map[string]*ImageRecord{
			"img_abcdef1111111111": {ID: "img_abcdef1111111111"},
			"img_abcdef2222222222": {ID: "img_abcdef2222222222"},
		},
	}

	if _, err := idx.resolve("img_abcdef"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ambiguous ref, got %v", err)
	}
}
