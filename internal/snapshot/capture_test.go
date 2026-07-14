package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestCaptureStoppedCopiesWritableDisksAndWritesManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "cow.ext4")
	content := []byte("snapshot payload")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	build, err := store.Reserve(context.Background(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	rec := &vmstore.VMRecord{
		ID: "kb_capture", Name: "source", Image: &vmstore.ImageRef{ID: "img_oci", Digest: "sha256:manifest"},
		StorageConfigs: []vmstore.StorageConfig{{
			ID: "cow", Role: vmstore.StorageRoleCOW, Path: source, Format: "raw", Filesystem: "ext4",
			Base: &vmstore.StorageBase{Family: "oci", ImageID: "img_oci", Digest: "sha256:manifest", LayerDigests: []string{"sha256:layer"}},
		}},
	}
	manifest, size, err := CaptureStopped(context.Background(), build, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Disks) != 1 || manifest.Consistency != "stopped-disk" || size <= 0 {
		t.Fatalf("manifest = %+v, size = %d", manifest, size)
	}
	sum := sha256.Sum256(content)
	if manifest.Disks[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %s", manifest.Disks[0].SHA256)
	}
	raw, err := os.ReadFile(filepath.Join(build.Record().StagingDir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Manifest
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Disks[0].Path != "disks/cow.ext4" {
		t.Fatalf("payload path = %s", persisted.Disks[0].Path)
	}
}
