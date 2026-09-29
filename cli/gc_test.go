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
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
)

func TestGCEmptyRootReportsNoActions(t *testing.T) {
	base := t.TempDir()
	args := []string{
		"--root-dir", filepath.Join(base, "data"), "--run-dir", filepath.Join(base, "run"),
		"--log-dir", filepath.Join(base, "log"), "gc", "--json",
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
	base := t.TempDir()
	roots := []string{
		"--root-dir", filepath.Join(base, "data"), "--run-dir", filepath.Join(base, "run"),
		"--log-dir", filepath.Join(base, "log"),
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
	base := t.TempDir()
	configuration := config.Default()
	configuration.Paths = storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	service, err := core.OpenSnapshots(t.Context(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	if err := supervise(ctx, service, 10*time.Millisecond, 0, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
