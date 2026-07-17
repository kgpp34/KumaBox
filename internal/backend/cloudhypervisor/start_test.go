package cloudhypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartProcessReportsEarlyProcessExit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stderrPath := filepath.Join(dir, "stderr.log")
	startedAt := time.Now()
	_, err := startProcess(Config{
		Binary:       "/bin/sh",
		Args:         []string{"-c", "echo deliberate-start-failure >&2; exit 42"},
		APISocket:    filepath.Join(dir, "ch.sock"),
		APITimeoutMs: 5000,
		PIDFile:      filepath.Join(dir, "ch.pid"),
		StdoutLog:    filepath.Join(dir, "stdout.log"),
		StderrLog:    stderrPath,
	})
	if err == nil {
		t.Fatal("startProcess() error = nil, want early process exit")
	}
	if !strings.Contains(err.Error(), "exited before API socket became ready: exit status 42") {
		t.Fatalf("startProcess() error = %q", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
		t.Fatalf("startProcess() reported early exit after %s", elapsed)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ch.pid")); !os.IsNotExist(statErr) {
		t.Fatalf("pid file error = %v, want not exist", statErr)
	}
	raw, readErr := os.ReadFile(stderrPath)
	if readErr != nil {
		t.Fatalf("read stderr log: %v", readErr)
	}
	if !strings.Contains(string(raw), "deliberate-start-failure") {
		t.Fatalf("stderr log = %q", raw)
	}
}
