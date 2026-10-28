package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/daemon"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
)

func TestGCEmptyRootReportsNoActions(t *testing.T) {
	base := shortTempDir(t)
	// Use an unused cgroup pathname without creating or scanning host scopes.
	t.Setenv("KUMABOX_VMM_CGROUP_PARENT", filepath.Join("/sys/fs/cgroup", "kumabox-test-"+filepath.Base(filepath.Dir(base))))
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	cfg.VMM.CgroupParent = filepath.Join("/sys/fs/cgroup", "kumabox-test-"+filepath.Base(filepath.Dir(base)))
	startTestNode(t, cfg)
	args := []string{
		"--run-dir", filepath.Join(base, "run"),
		"gc", "--json",
	}
	var output bytes.Buffer
	if err := Execute(t.Context(), args, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var report core.GCReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Actions == nil || len(report.Actions) != 0 || report.Skipped != 0 {
		t.Fatalf("empty GC report = %+v", report)
	}
}

func TestGCSnapshotPolicyFlags(t *testing.T) {
	base := shortTempDir(t)
	// Use an unused cgroup pathname without creating or scanning host scopes.
	t.Setenv("KUMABOX_VMM_CGROUP_PARENT", filepath.Join("/sys/fs/cgroup", "kumabox-test-"+filepath.Base(filepath.Dir(base))))
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	cfg.VMM.CgroupParent = filepath.Join("/sys/fs/cgroup", "kumabox-test-"+filepath.Base(filepath.Dir(base)))
	startTestNode(t, cfg)
	roots := []string{
		"--run-dir", filepath.Join(base, "run"),
	}
	for _, flags := range [][]string{
		{"--snapshot-keep", "1"},
		{"--snapshot-age", "24h"},
		{"--snapshot-size", "1GB"},
		{"--snapshot-dry-run"},
		{"--snapshot", "--snapshot-keep", "-1"},
		{"--snapshot", "--snapshot-size", "bad"},
	} {
		args := append(append([]string{}, roots...), "gc")
		args = append(args, flags...)
		err := Execute(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{})
		if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
			t.Fatalf("gc flags %v error = %v", flags, err)
		}
	}
	args := append(append([]string{}, roots...), "gc", "--snapshot", "--snapshot-size", "1GB", "--snapshot-dry-run", "--json")
	var output bytes.Buffer
	if err := Execute(t.Context(), args, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var report core.GCReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || len(report.Actions) != 0 {
		t.Fatalf("empty snapshot preview = %+v, %v", report, err)
	}
}

func TestDaemonStopsWhenContextIsCancelled(t *testing.T) {
	base := shortTempDir(t)
	configuration := config.Default()
	configuration.Paths = storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	service, err := core.OpenMaintenance(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	if err := daemon.Supervise(ctx, service, 10*time.Millisecond, 0, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
