package gc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
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
