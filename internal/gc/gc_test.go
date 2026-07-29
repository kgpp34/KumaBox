package gc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestDryRunReportsSnapshotAndStorageOrphansButProtectsLeasedPending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	orphanStorage := filepath.Join(cfg.Runtime.RootDir, "storage", "vms", "kb_orphan")
	orphanStaging := filepath.Join(cfg.Runtime.RootDir, "snapshot", "staging", "capture-orphan")
	for _, path := range []string{orphanStorage, orphanStaging} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphanStaging, old, old); err != nil {
		t.Fatal(err)
	}
	build, err := snapshot.NewStore(cfg.Runtime.RootDir).Reserve(context.Background(), "active-build")
	if err != nil {
		t.Fatal(err)
	}
	defer build.Abort() //nolint:errcheck
	indexPath := filepath.Join(cfg.Runtime.RootDir, "snapshot", "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]any
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	snapshots := index["snapshots"].(map[string]any)
	record := snapshots[build.Record().ID].(map[string]any)
	record["updatedAt"] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	raw, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, orphanStorage, "orphan_vm_storage")
	assertCandidate(t, report, orphanStaging, "orphan_snapshot_staging")
	assertNoCandidate(t, report, build.Record().StagingDir)
}

func TestDryRunProtectsNativeSnapshotAssetsAndExplainsStaleStaging(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	const imageID = "img_snapshot_only"
	manifestDigest := "sha256:" + strings.Repeat("d", 64)
	layerDigest := "sha256:" + strings.Repeat("a", 64)
	kernelDigest := "sha256:" + strings.Repeat("b", 64)
	initrdDigest := "sha256:" + strings.Repeat("c", 64)
	manifest := snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v2", Type: "native", Consistency: "crash",
		Source: snapshot.Source{VMID: "kb_deleted", ImageID: imageID},
		Base:   &snapshot.Base{Family: "oci", ImageID: imageID, Digest: manifestDigest, LayerDigests: []string{layerDigest}},
		Boot:   &snapshot.BootManifest{KernelDigest: kernelDigest, InitrdDigest: initrdDigest},
	}
	ready := createGCReadySnapshot(t, cfg.Runtime.RootDir, "native-live", manifest)

	imageDir := filepath.Join(cfg.Runtime.RootDir, "cloudimg", imageID)
	layerPath := filepath.Join(cfg.Runtime.RootDir, "oci", "erofs", "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:")+".erofs")
	kernelPath := filepath.Join(cfg.Runtime.RootDir, "oci", "boot", "blobs", "sha256", strings.TrimPrefix(kernelDigest, "sha256:"))
	initrdPath := filepath.Join(cfg.Runtime.RootDir, "oci", "boot", "blobs", "sha256", strings.TrimPrefix(initrdDigest, "sha256:"))
	contentPath := filepath.Join(cfg.Runtime.RootDir, "oci", "content", "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:"))
	manifestContentPath := filepath.Join(cfg.Runtime.RootDir, "oci", "content", "blobs", "sha256", strings.TrimPrefix(manifestDigest, "sha256:"))
	for _, path := range []string{filepath.Join(imageDir, "base.qcow2"), layerPath, kernelPath, initrdPath, contentPath, manifestContentPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	vmStore := vmstore.New(cfg.Runtime.RootDir)
	vm, err := vmStore.Create(vmstore.CreateRequest{
		Name: "restore-staging", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleRestore := filepath.Join(vm.RunDir, ".restore-staging")
	staleOrphan := filepath.Join(cfg.Runtime.RootDir, "snapshot", "staging", "orphan-old")
	freshOrphan := filepath.Join(cfg.Runtime.RootDir, "snapshot", "staging", "orphan-fresh")
	for _, path := range []string{staleRestore, staleOrphan, freshOrphan} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{staleRestore, staleOrphan} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, staleRestore, "stale_restore_staging")
	assertCandidate(t, report, staleOrphan, "orphan_snapshot_staging")
	assertNoCandidate(t, report, freshOrphan)
	for _, protected := range []string{ready.DataDir, imageDir, layerPath, kernelPath, initrdPath, contentPath, manifestContentPath} {
		assertNoCandidate(t, report, protected)
	}
}

func TestDryRunFailsClosedForCorruptReadyNativeManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	ready := createGCReadySnapshot(t, cfg.Runtime.RootDir, "corrupt-native", snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v2", Type: "native", Consistency: "crash",
	})
	if err := os.WriteFile(filepath.Join(ready.DataDir, "snapshot.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DryRun(cfg); err == nil || !strings.Contains(err.Error(), "read ready snapshot") {
		t.Fatalf("dry-run error = %v", err)
	}
}

func TestDryRunFailsClosedForInvalidNativeSnapshotReference(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	createGCReadySnapshot(t, cfg.Runtime.RootDir, "invalid-reference", snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v2", Type: "native", Consistency: "crash",
		Base: &snapshot.Base{Family: "oci", LayerDigests: []string{"sha256:not-a-digest"}},
	})
	if _, err := DryRun(cfg); err == nil || !strings.Contains(err.Error(), "base layer digest") {
		t.Fatalf("dry-run error = %v", err)
	}
}

func createGCReadySnapshot(t *testing.T, rootDir, name string, manifest snapshot.Manifest) *snapshot.Record {
	t.Helper()
	store := snapshot.NewStore(rootDir)
	build, err := store.Reserve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	rec := build.Record()
	manifest.ID = rec.ID
	manifest.Name = rec.Name
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.StagingDir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func TestDryRunReportsOnlyManagedCandidates(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	rootDisk := filepath.Join(dir, "fixtures", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(rootDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDisk, []byte("root disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := vmstore.New(cfg.Runtime.RootDir)
	rec, err := store.Create(vmstore.CreateRequest{
		Name:     "gc",
		RootDisk: rootDisk,
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   cfg.Runtime.RunDir,
		LogDir:   cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec.RunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.RunDir, "ch.pid"), []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.RunDir, "vsock.uds"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	orphanRun := filepath.Join(cfg.Runtime.RunDir, "vms", "orphan")
	orphanLog := filepath.Join(cfg.Runtime.LogDir, "vms", "orphan")
	if err := os.MkdirAll(orphanRun, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(orphanLog, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, filepath.Join(rec.RunDir, "ch.pid"), "stale_runtime_file")
	assertCandidate(t, report, filepath.Join(rec.RunDir, "vsock.uds"), "stale_agent_socket")
	assertCandidate(t, report, orphanRun, "orphan_run_dir")
	assertCandidate(t, report, orphanLog, "orphan_log_dir")
	for _, candidate := range report.Candidates {
		if candidate.Path == rootDisk {
			t.Fatalf("root disk must not be a GC candidate: %+v", candidate)
		}
	}
}

func TestRepairRemovesOrphanManagedStorage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	orphan := filepath.Join(cfg.Runtime.RootDir, "storage", "vms", "kb_orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "cow.ext4"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Repair(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan storage still exists, stat error = %v", err)
	}
	assertCandidate(t, report, orphan, "orphan_vm_storage")
	if report.DryRun {
		t.Fatal("repair report is marked dry-run")
	}
}

func TestDryRunReportsImageCandidates(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	imageStore := imagestore.New(cfg.Runtime.RootDir)
	indexed, err := imageStore.Create(imagestore.CreateRequest{
		Name:   "indexed",
		Source: imagestore.Source{Type: "test", URI: "fixtures/indexed.img"},
		RootDisk: imagestore.RootDisk{
			Path:   filepath.Join(cfg.Runtime.RootDir, "cloudimg", "img_indexed", "base.qcow2"),
			Format: "qcow2",
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.RootDir, "cloudimg", indexed.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(cfg.Runtime.RootDir, "cloudimg", "staging", "import-deadbeef")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(cfg.Runtime.RootDir, "cloudimg", "img_orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	vmReferencedMissingFromIndex := filepath.Join(cfg.Runtime.RootDir, "cloudimg", "img_live_missing")
	if err := os.MkdirAll(vmReferencedMissingFromIndex, 0o755); err != nil {
		t.Fatal(err)
	}

	vmStore := vmstore.New(cfg.Runtime.RootDir)
	_, err = vmStore.Create(vmstore.CreateRequest{
		Name:     "live-image",
		RootDisk: filepath.Join(vmReferencedMissingFromIndex, "base.qcow2"),
		Firmware: "CLOUDHV.fd",
		Image: &vmstore.ImageRef{
			ID:       "img_live_missing",
			Name:     "missing",
			RootDisk: filepath.Join(vmReferencedMissingFromIndex, "base.qcow2"),
			BootMode: "uefi",
		},
		RunDir: cfg.Runtime.RunDir,
		LogDir: cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, staging, "image_staging_dir")
	assertCandidate(t, report, orphan, "orphan_image_dir")
	assertNoCandidate(t, report, filepath.Join(cfg.Runtime.RootDir, "cloudimg", indexed.ID))
	assertNoCandidate(t, report, vmReferencedMissingFromIndex)
}

func TestDryRunReportsOCICandidates(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	liveKernel := filepath.Join(cfg.Runtime.RootDir, "oci", "boot", "blobs", "sha256", strings.Repeat("1", 64))
	liveInitrd := filepath.Join(cfg.Runtime.RootDir, "oci", "boot", "blobs", "sha256", strings.Repeat("2", 64))
	orphanBoot := filepath.Join(cfg.Runtime.RootDir, "oci", "boot", "blobs", "sha256", strings.Repeat("3", 64))
	liveEROFS := filepath.Join(cfg.Runtime.RootDir, "oci", "erofs", "blobs", "sha256", strings.Repeat("4", 64)+".erofs")
	orphanEROFS := filepath.Join(cfg.Runtime.RootDir, "oci", "erofs", "blobs", "sha256", strings.Repeat("5", 64)+".erofs")
	liveContent := filepath.Join(cfg.Runtime.RootDir, "oci", "content", "blobs", "sha256", strings.Repeat("6", 64))
	orphanContent := filepath.Join(cfg.Runtime.RootDir, "oci", "content", "blobs", "sha256", strings.Repeat("7", 64))
	contentStage := filepath.Join(cfg.Runtime.RootDir, "oci", "content", "staging", "blob-deadbeef")
	buildStage := filepath.Join(cfg.Runtime.RootDir, "oci", "staging", "erofs-deadbeef")
	for _, path := range []string{
		liveKernel, liveInitrd, orphanBoot, liveEROFS, orphanEROFS, liveContent, orphanContent, contentStage, filepath.Join(buildStage, "layer.tar"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := imagestore.New(cfg.Runtime.RootDir).Create(imagestore.CreateRequest{
		Name:   "oci-live",
		Source: imagestore.Source{Type: "oci", URI: "example.com/live@sha256:test"},
		Boot: imagestore.Boot{
			Mode:   "direct",
			Kernel: liveKernel,
			Initrd: liveInitrd,
		},
		OCI: &imagestore.OCI{
			Ref: "example.com/live:latest",
			Config: imagestore.OCIDescriptor{
				Digest: "sha256:" + strings.Repeat("6", 64),
			},
			Layers: []imagestore.OCILayer{
				{
					Digest: "sha256:" + strings.Repeat("6", 64),
					EROFS: &imagestore.EROFSLayer{
						Path:       liveEROFS,
						Filesystem: "erofs",
						Digest:     "sha256:" + strings.Repeat("8", 64),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, orphanBoot, "orphan_boot_asset")
	assertCandidate(t, report, orphanEROFS, "orphan_erofs_blob")
	assertCandidate(t, report, orphanContent, "orphan_content_blob")
	assertCandidate(t, report, contentStage, "oci_content_staging")
	assertCandidate(t, report, buildStage, "oci_build_staging")
	assertNoCandidate(t, report, liveKernel)
	assertNoCandidate(t, report, liveInitrd)
	assertNoCandidate(t, report, liveEROFS)
	assertNoCandidate(t, report, liveContent)
}

func TestDryRunFailsWhenImageIndexIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	indexPath := filepath.Join(cfg.Runtime.RootDir, "cloudimg", "index.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := DryRun(cfg); err == nil {
		t.Fatal("expected corrupt image index error")
	}
}

func TestDryRunReportsNetworkPendingAndOrphans(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	vmStore := vmstore.New(cfg.Runtime.RootDir)
	rec, err := vmStore.Create(vmstore.CreateRequest{
		Name:     "network-live",
		RootDisk: "base.qcow2",
		Firmware: "CLOUDHV.fd",
		RunDir:   cfg.Runtime.RunDir,
		LogDir:   cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	liveAlloc, err := kbnetwork.NewAllocator(cfg.Runtime.RootDir, cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    rec.ID,
		Network: "default",
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	networkStore := kbnetwork.NewStore(cfg.Runtime.RootDir)
	if err := networkStore.UpsertRecord(liveAlloc.Record); err != nil {
		t.Fatal(err)
	}
	if _, err := vmStore.SetNetworkConfigs(rec.ID, []kbnetwork.Config{liveAlloc.Config}); err != nil {
		t.Fatal(err)
	}

	pending := kbnetwork.Record{
		ID:        "net_pending",
		VMID:      "kb_missing",
		Network:   "default",
		Provider:  kbnetwork.ProviderHostTap,
		IfName:    "eth0",
		TAP:       "kbtappending",
		MAC:       "5a:00:00:00:00:10",
		BridgeDev: cfg.Network.Bridge,
		IPs:       []string{"10.88.0.42/16"},
		Gateway:   cfg.Network.Gateway,
		Cleanup: kbnetwork.Cleanup{
			Pending:       true,
			Reason:        "tap delete failed",
			LastAttemptAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := networkStore.UpsertRecord(pending); err != nil {
		t.Fatal(err)
	}
	orphanLease, err := kbnetwork.NewAllocator(cfg.Runtime.RootDir, cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    "kb_orphan",
		Network: "default",
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCandidate(t, report, liveAlloc.Record.TAP)
	assertCandidate(t, report, pending.ID, "pending_cleanup")
	assertCandidate(t, report, pending.TAP, "stale_tap")
	assertCandidate(t, report, orphanLease.Config.Network.IP, "orphan_lease")
}

func TestDryRunReportsNetworkDriftWithoutDeleteGuess(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	vmStore := vmstore.New(cfg.Runtime.RootDir)
	rec, err := vmStore.Create(vmstore.CreateRequest{
		Name:     "network-drift",
		RootDisk: "base.qcow2",
		Firmware: "CLOUDHV.fd",
		RunDir:   cfg.Runtime.RunDir,
		LogDir:   cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	alloc, err := kbnetwork.NewAllocator(cfg.Runtime.RootDir, cfg.Network).Allocate(kbnetwork.AllocateRequest{
		VMID:    rec.ID,
		Network: "default",
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	drifted := alloc.Record
	drifted.MAC = "5a:00:00:00:00:99"
	if err := kbnetwork.NewStore(cfg.Runtime.RootDir).UpsertRecord(drifted); err != nil {
		t.Fatal(err)
	}
	if _, err := vmStore.SetNetworkConfigs(rec.ID, []kbnetwork.Config{alloc.Config}); err != nil {
		t.Fatal(err)
	}

	report, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidate(t, report, alloc.Record.ID, "network_drift")
	assertNoCandidate(t, report, alloc.Record.TAP)
}

func TestDryRunFailsWhenNetworkStateIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	indexPath := filepath.Join(cfg.Runtime.RootDir, "network", "index.json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := DryRun(cfg); err == nil {
		t.Fatal("expected corrupt network index error")
	}
}

func TestDryRunFailsWhenNetworkLeasesAreCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	leasePath := filepath.Join(cfg.Runtime.RootDir, "network", "leases.json")
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := DryRun(cfg); err == nil {
		t.Fatal("expected corrupt network lease error")
	}
}

func TestNetworkCandidatesDetectDriftAndOrphanLease(t *testing.T) {
	t.Parallel()
	vm := &vmstore.VMRecord{
		ID: "vm-live",
		NetworkConfigs: []kbnetwork.Config{{
			ID: "net-live", TAP: "tap-live", MAC: "02:00:00:00:00:01",
			Backend: kbnetwork.ProviderHostTap, BridgeDev: "kumabox0",
			Network: &kbnetwork.GuestInfo{IP: "10.88.0.2", Gateway: "10.88.0.1"},
		}},
	}
	records := []kbnetwork.Record{
		{ID: "net-live", VMID: "vm-live", TAP: "tap-live", MAC: "02:00:00:00:00:99", Provider: kbnetwork.ProviderHostTap, BridgeDev: "kumabox0", IPs: []string{"10.88.0.2/16"}, Gateway: "10.88.0.1"},
		{ID: "net-missing-vm", VMID: "vm-gone", TAP: "tap-gone", Provider: kbnetwork.ProviderHostTap},
	}
	leases := map[string]kbnetwork.Lease{
		"10.88.0.99": {VMID: "vm-gone", TAP: "tap-gone"},
		"10.88.0.2":  {VMID: "vm-live", TAP: "tap-live"},
	}
	candidates := networkCandidates([]*vmstore.VMRecord{vm}, records, leases)
	assertCandidateList(t, candidates, "net-live", "network_drift")
	assertCandidateList(t, candidates, "tap-gone", "stale_tap")
	assertCandidateList(t, candidates, "10.88.0.99", "orphan_lease")
	assertNoCandidateList(t, candidates, "10.88.0.2")
}

func TestImageCandidatesProtectIndexedAndLiveImages(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cloudimg := filepath.Join(root, "cloudimg")
	for _, name := range []string{"staging/import-1", "img-indexed", "img-live", "img-orphan"} {
		if err := os.MkdirAll(filepath.Join(cloudimg, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	images := []*imagestore.ImageRecord{{ID: "img-indexed"}}
	candidates := imageCandidates(root, images, map[string]struct{}{"img-live": {}})
	assertCandidateList(t, candidates, filepath.Join(cloudimg, "staging/import-1"), "image_staging_dir")
	assertCandidateList(t, candidates, filepath.Join(cloudimg, "img-orphan"), "orphan_image_dir")
	assertNoCandidateList(t, candidates, filepath.Join(cloudimg, "img-indexed"))
	assertNoCandidateList(t, candidates, filepath.Join(cloudimg, "img-live"))
}

func assertCandidateList(t *testing.T, candidates []Candidate, path, typ string) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.Path == path && candidate.Type == typ {
			return
		}
	}
	t.Fatalf("missing candidate %s %s in %+v", typ, path, candidates)
}

func assertNoCandidateList(t *testing.T, candidates []Candidate, path string) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.Path == path {
			t.Fatalf("unexpected candidate for %s: %+v", path, candidate)
		}
	}
}

func assertCandidate(t *testing.T, report *Report, path string, typ string) {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.Path == path && candidate.Type == typ && candidate.Component != "" && candidate.Reason != "" {
			return
		}
	}
	t.Fatalf("missing candidate %s %s in %+v", typ, path, report.Candidates)
}

func assertNoCandidate(t *testing.T, report *Report, path string) {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.Path == path {
			t.Fatalf("unexpected candidate for %s: %+v", path, candidate)
		}
	}
}
