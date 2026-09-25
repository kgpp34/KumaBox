package vmm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestLogsTailUsesLinesAndPreservesTrailingNewline(t *testing.T) {
	paths := testLogPaths(t)
	writeTestLog(t, paths, "one\ntwo\nthree\n")
	var output bytes.Buffer
	if err := paths.Logs(t.Context(), testSandboxID, LogOptions{Tail: 2}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "two\nthree\n" {
		t.Fatalf("tail output = %q", output.String())
	}

	output.Reset()
	if err := paths.Logs(t.Context(), testSandboxID, LogOptions{}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "one\ntwo\nthree\n" {
		t.Fatalf("complete output = %q", output.String())
	}
}

func TestLogsFollowRewindsTruncatedFileAndCancelsCleanly(t *testing.T) {
	paths := testLogPaths(t)
	path := writeTestLog(t, paths, "old-one\nold-two\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- paths.Logs(ctx, testSandboxID, LogOptions{Tail: 1, Follow: true}, &output)
	}()
	waitForLog(t, &output, "old-two\n")
	if err := os.WriteFile(path, []byte("new-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, &output, "old-two\nnew-boot\n")
	replacement := filepath.Join(filepath.Dir(path), "replacement.log")
	if err := os.WriteFile(replacement, []byte("replacement-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, &output, "old-two\nnew-boot\nreplacement-boot\n")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not stop after cancellation")
	}
}

func TestLogsMissingAndRemovalOwnership(t *testing.T) {
	paths := testLogPaths(t)
	var output bytes.Buffer
	if err := paths.Logs(t.Context(), testSandboxID, LogOptions{}, &output); err == nil {
		t.Fatal("Logs opened a missing VMM log")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactUnavailable {
		t.Fatalf("missing log error = %v", err)
	}
	writeTestLog(t, paths, "diagnostic\n")
	runDir, _ := paths.RunDir(testSandboxID)
	if err := paths.RemoveLogs(t.Context(), testSandboxID); err != nil {
		t.Fatal(err)
	}
	if err := paths.RemoveLogs(t.Context(), testSandboxID); err != nil {
		t.Fatalf("idempotent RemoveLogs = %v", err)
	}
	logDir, _ := paths.LogDir(testSandboxID)
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("log directory remains: %v", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("runtime directory was removed with logs: %v", err)
	}
}

func testLogPaths(t *testing.T) Paths {
	t.Helper()
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Prepare(testSandboxID); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeTestLog(t *testing.T, paths Paths, content string) string {
	t.Helper()
	path, err := paths.LogFile(testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForLog(t *testing.T, output *lockedBuffer, expected string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), expected) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log output %q never contained %q", output.String(), expected)
}

func TestLogsPreservesWriterFailure(t *testing.T) {
	paths := testLogPaths(t)
	writeTestLog(t, paths, "output\n")
	failure := errors.New("closed output")
	if err := paths.Logs(t.Context(), testSandboxID, LogOptions{}, failingLogWriter{failure}); !errors.Is(err, failure) {
		t.Fatalf("writer failure = %v", err)
	}
}

type failingLogWriter struct{ error }

func (w failingLogWriter) Write([]byte) (int, error) { return 0, w.error }
