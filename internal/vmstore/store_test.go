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
	if rec.Config != filepath.Join(rec.RunDir, "cloud-hypervisor.json") {
		t.Fatalf("config path = %s", rec.Config)
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

	indexPath := filepath.Join(dir, "data", "backends", backendCloudHypervisor, "index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRemovesRecordAndName(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Create(CreateRequest{
		Name:     "delete-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect("delete-me"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect after delete error = %v", err)
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

func TestCreateSupportsFirmwareBoot(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "uefi",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Firmware == "" {
		t.Fatal("expected firmware path")
	}
	if rec.Kernel != "" || rec.Initrd != "" {
		t.Fatalf("unexpected direct boot fields: kernel=%q initrd=%q", rec.Kernel, rec.Initrd)
	}
	if rec.Metadata == nil {
		t.Fatal("expected NoCloud metadata")
	}
	if rec.Metadata.Type != "nocloud" {
		t.Fatalf("metadata type = %s", rec.Metadata.Type)
	}
	if rec.Metadata.CidataDir != filepath.Join(rec.RunDir, "cidata") {
		t.Fatalf("cidata dir = %s", rec.Metadata.CidataDir)
	}
	if rec.Metadata.CidataDisk != filepath.Join(rec.RunDir, "cidata.img") {
		t.Fatalf("cidata disk = %s", rec.Metadata.CidataDisk)
	}
}

func TestCreateRejectsMixedFirmwareAndDirectBoot(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	_, err := store.Create(CreateRequest{
		Name:     "mixed",
		RootDisk: "ubuntu.img",
		Kernel:   "vmlinuz",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err == nil {
		t.Fatal("expected mixed boot error")
	}
}

func TestResolveByIDPrefix(t *testing.T) {
	idx := &vmIndex{
		VMs: map[string]*VMRecord{
			"kb_abcdef": {ID: "kb_abcdef"},
		},
		Names: map[string]string{},
	}

	id, err := idx.resolve("kb_abc")
	if err != nil {
		t.Fatal(err)
	}
	if id != "kb_abcdef" {
		t.Fatalf("id = %s", id)
	}
}
