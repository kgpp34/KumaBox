package gc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
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

func assertCandidate(t *testing.T, report *Report, path string, typ string) {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.Path == path && candidate.Type == typ && candidate.Reason != "" {
			return
		}
	}
	t.Fatalf("missing candidate %s %s in %+v", typ, path, report.Candidates)
}
