package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreImportValidatesAndAssignsNewIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewStore(root)
	packagePath, sourceID := exportTestPackage(t, store, "source", false)
	fakeQEMU := fakeImportQEMUImg(t, root)
	imported, err := store.Import(context.Background(), ImportOptions{Input: packagePath, Name: "imported", QEMUImgBinary: fakeQEMU})
	if err != nil {
		t.Fatal(err)
	}
	if imported.ID == sourceID || imported.Name != "imported" || imported.State != StateReady {
		t.Fatalf("imported = %+v", imported)
	}
	manifest, err := store.LoadManifest(context.Background(), imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != imported.ID || manifest.Name != imported.Name {
		t.Fatalf("manifest identity = %+v", manifest)
	}
}

func TestStoreImportRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewStore(root)
	packagePath, _ := exportTestPackage(t, store, "bad-source", true)
	if _, err := store.Import(context.Background(), ImportOptions{Input: packagePath, Name: "bad-import", QEMUImgBinary: fakeImportQEMUImg(t, root)}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("CHECKSUM_MISMATCH")) {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Inspect("bad-import"); err == nil {
		t.Fatal("failed import published a snapshot")
	}
}

func TestStoreImportRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	packagePath := filepath.Join(root, "unsafe.kbsnap")
	f, err := os.Create(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	data := []byte("bad")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(data)
	_ = tw.Close()
	_ = f.Close()
	if _, err := NewStore(root).Import(context.Background(), ImportOptions{Input: packagePath, Name: "unsafe", QEMUImgBinary: "qemu-img"}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("ARCHIVE_UNSAFE")) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatalf("escape path created: %v", err)
	}
}

func exportTestPackage(t *testing.T, store *Store, name string, badChecksum bool) (string, string) {
	t.Helper()
	build, err := store.Reserve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	staging := build.Record().StagingDir
	_ = os.MkdirAll(filepath.Join(staging, "disks"), 0o700)
	content := []byte("qcow2-test-payload")
	diskPath := filepath.Join(staging, "disks", "root.qcow2")
	if err := os.WriteFile(diskPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if badChecksum {
		digest = string(bytes.Repeat([]byte("0"), 64))
	}
	manifest := Manifest{SchemaVersion: "kumabox.snapshot.v1", ID: build.Record().ID, Name: name, Type: "disk", Consistency: "stopped-disk", Disks: []DiskManifest{{ID: "root", Role: "cow", Path: "disks/root.qcow2", Format: "qcow2", VirtualSizeBytes: int64(len(content)), AllocatedSizeBytes: int64(len(content)), SHA256: digest, CopyStrategy: "stream"}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(staging, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(store.rootDir), name+".kbsnap")
	if err := store.Export(context.Background(), ready.ID, ExportOptions{Output: output}); err != nil {
		t.Fatal(err)
	}
	return output, ready.ID
}

func fakeImportQEMUImg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img-import")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"format\":\"qcow2\",\"virtual-size\":1024}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
