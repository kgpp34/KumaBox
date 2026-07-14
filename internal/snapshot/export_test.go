package snapshot

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreExportWritesManifestFirstAndSparseMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewStore(root)
	build, err := store.Reserve(context.Background(), "export")
	if err != nil {
		t.Fatal(err)
	}
	staging := build.Record().StagingDir
	if err := os.MkdirAll(filepath.Join(staging, "disks"), 0o700); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(staging, "disks", "cow.ext4")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("data"), 512*1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schemaVersion":"kumabox.snapshot.v1","id":"` + build.Record().ID + `","name":"export","type":"disk","consistency":"stopped-disk","disks":[{"id":"cow","role":"cow","path":"disks/cow.ext4","format":"raw","virtualSizeBytes":1048576,"allocatedSizeBytes":4096,"sha256":"test","copyStrategy":"sparse"}]}`)
	if err := os.WriteFile(filepath.Join(staging, "snapshot.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(4096)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "out.kbsnap")
	if err := store.Export(context.Background(), ready.ID, ExportOptions{Output: output}); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close() //nolint:errcheck
	tr := tar.NewReader(archive)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "manifest.json" {
		t.Fatalf("first entry = %s", hdr.Name)
	}
	if _, err := io.Copy(io.Discard, tr); err != nil {
		t.Fatal(err)
	}
	hdr, err = tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "disks/cow.ext4" || hdr.PAXRecords[paxSparseSize] != "1048576" {
		t.Fatalf("disk header = %+v", hdr)
	}
}
