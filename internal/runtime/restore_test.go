package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestRestoreSnapshotCreatesIndependentOCIVM(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	layerPath := filepath.Join(dir, "layer.erofs")
	if err := os.WriteFile(layerPath, []byte("layer"), 0o600); err != nil {
		t.Fatal(err)
	}

	const manifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const layerDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name: "restore-image",
		Boot: imagestore.Boot{Mode: "direct", Kernel: filepath.Join(dir, "vmlinuz"), Initrd: filepath.Join(dir, "initrd"), Cmdline: "console=ttyS0"},
		OCI: &imagestore.OCI{
			DigestRef: "example.invalid/image@" + manifestDigest,
			Layers: []imagestore.OCILayer{{
				Index: 0, Digest: layerDigest,
				EROFS: &imagestore.EROFSLayer{Path: layerPath, Filesystem: "erofs", SizeBytes: 5, SourceLayer: layerDigest},
			}},
			BuiltAt: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 4096)
	copy(payload, "restored writable state")
	sum := sha256.Sum256(payload)
	snapshotStore := snapshot.NewStore(rootDir)
	build, err := snapshotStore.Reserve(context.Background(), "restore-source")
	if err != nil {
		t.Fatal(err)
	}
	staging := build.Record().StagingDir
	if err := os.MkdirAll(filepath.Join(staging, "disks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "disks", "cow.ext4"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v1", ID: build.Record().ID, Name: "restore-source",
		Type: "disk", Consistency: "stopped-disk",
		Source: snapshot.Source{VMID: "source-vm", VMName: "source", ImageID: image.ID, ImageDigest: manifestDigest},
		Base:   &snapshot.Base{Family: "oci", ImageID: image.ID, Digest: manifestDigest, LayerDigests: []string{layerDigest}},
		Disks: []snapshot.DiskManifest{{
			ID: "cow", Role: "cow", Path: "disks/cow.ext4", Format: "raw", Filesystem: "ext4",
			VirtualSizeBytes: int64(len(payload)), AllocatedSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]), CopyStrategy: "stream-copy",
		}},
		CreatedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	rt := NewWithBackend(vmstore.New(rootDir), backendFake{render: func(*vmstore.VMRecord) error { return nil }})
	rt.cfg = cfg

	restored, err := rt.RestoreSnapshot(context.Background(), ready.ID, RestoreOptions{Name: "restored", CPUs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != vmstore.StateCreated || restored.ID == manifest.Source.VMID {
		t.Fatalf("restored identity/state = %s/%s", restored.ID, restored.State)
	}
	if restored.CPUs != 2 || restored.Network != "none" {
		t.Fatalf("restored runtime options = cpus %d network %s", restored.CPUs, restored.Network)
	}
	if len(restored.StorageConfigs) != 2 {
		t.Fatalf("storage count = %d, want layer and COW", len(restored.StorageConfigs))
	}
	got, err := os.ReadFile(restored.StorageConfigs[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("restored COW payload differs from snapshot")
	}
	wantOwner := filepath.Join(rootDir, "storage", "vms", restored.ID)
	if filepath.Dir(restored.StorageConfigs[1].Path) != wantOwner {
		t.Fatalf("restored COW path = %s, want owner %s", restored.StorageConfigs[1].Path, wantOwner)
	}
}

func TestSnapshotDiskPathRejectsTraversalAndSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(regular, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotDiskPath(dir, "disk.raw"); err != nil {
		t.Fatalf("regular disk rejected: %v", err)
	}
	if _, err := snapshotDiskPath(dir, "../disk.raw"); err == nil {
		t.Fatal("traversal path was accepted")
	}
	if err := os.Symlink(regular, filepath.Join(dir, "link.raw")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotDiskPath(dir, "link.raw"); err == nil {
		t.Fatal("symlink disk was accepted")
	}
}
