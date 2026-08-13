package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotDirectoryExportImport(t *testing.T) {
	root := t.TempDir()
	sourceStore := NewStore(filepath.Join(root, "source"))
	ready := createImportFixture(t, sourceStore, "source")
	exported := filepath.Join(root, "exported")
	if err := sourceStore.ExportDirectory(t.Context(), ready.ID, exported); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(exported, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	destinationStore := NewStore(filepath.Join(root, "destination"))
	imported, err := destinationStore.ImportDirectory(t.Context(), exported, "imported", fakeImportQEMUImg(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Name != "imported" || imported.ID == ready.ID {
		t.Fatalf("imported snapshot = %+v", imported)
	}
	manifest, err := destinationStore.LoadManifest(t.Context(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != imported.ID || manifest.Name != imported.Name {
		t.Fatalf("imported manifest identity = %+v", manifest)
	}
}

func TestSnapshotDirectoryRejectsExistingDestinationAndSymlink(t *testing.T) {
	store := NewStore(t.TempDir())
	ready := createImportFixture(t, store, "source")
	existing := t.TempDir()
	if err := store.ExportDirectory(t.Context(), ready.ID, existing); err == nil {
		t.Fatal("expected existing destination error")
	}
	source := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "snapshot.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.ImportDirectory(context.Background(), source, "unsafe", "qemu-img"); err == nil || !strings.Contains(err.Error(), "SNAPSHOT_UNSAFE") {
		t.Fatalf("symlink import error = %v", err)
	}
}

func createImportFixture(t *testing.T, store *Store, name string) *Record {
	t.Helper()
	content := []byte("directory-snapshot")
	build, err := store.Reserve(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	diskPath := filepath.Join(build.Record().StagingDir, "disks", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifest := Manifest{
		SchemaVersion: "kumabox.snapshot.v1", ID: build.Record().ID, Name: name,
		Type: "disk", Consistency: "stopped-disk",
		Disks: []DiskManifest{{
			ID: "root", Role: "cow", Path: "disks/root.qcow2", Format: "qcow2",
			VirtualSizeBytes: int64(len(content)), AllocatedSizeBytes: int64(len(content)),
			SHA256: hex.EncodeToString(sum[:]), CopyStrategy: "stream",
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build.Record().StagingDir, ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return ready
}
