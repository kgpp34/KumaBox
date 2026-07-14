package gc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

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
	assertCandidate(t, report, orphanRun, "orphan_run_dir")
	assertCandidate(t, report, orphanLog, "orphan_log_dir")
	for _, candidate := range report.Candidates {
		if candidate.Path == rootDisk {
			t.Fatalf("root disk must not be a GC candidate: %+v", candidate)
		}
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
