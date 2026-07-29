package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestInspectAgentStatusReportsStoppedVM(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "console.log"), []byte("booting\nagent failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	view := inspectAgentStatus(t.Context(), &vmstore.VMRecord{
		ID:     "kb_test",
		Name:   "stopped",
		State:  vmstore.StateStopped,
		LogDir: logDir,
	}, 0)
	if view.Ready || view.Readiness != "vm-not-running" {
		t.Fatalf("view = %+v", view)
	}
	if !strings.Contains(view.Diagnostics["consoleTail"], "agent failed") {
		t.Fatalf("diagnostics = %+v", view.Diagnostics)
	}
}

func TestReadLogTailKeepsLatestLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLogTail(path, 2); got != "two\nthree" {
		t.Fatalf("tail = %q", got)
	}
}
