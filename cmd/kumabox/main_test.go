package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/daemon"
	"github.com/kumabox/kumabox/internal/storage"
)

// TestMainBinaryStreamsAndExitCodes builds the real entrypoint so argument
// parsing, signal setup, diagnostic printing, and os.Exit remain in scope.
func TestMainBinaryStreamsAndExitCodes(t *testing.T) {
	binary := buildBinary(t)
	base := nodeTempDir(t)
	global := []string{
		"--run-dir", filepath.Join(base, "run"),
	}
	startBinaryTestNode(t, base)
	tests := []struct {
		name           string
		args           []string
		wantCode       int
		wantStdout     string
		stdoutContains string
		stderrContains string
	}{
		{name: "version JSON", args: []string{"version", "--json"}, stdoutContains: "\n  \"build_time\":"},
		{name: "image JSON", args: append(append([]string(nil), global...), "image", "ls", "--json"), wantStdout: "[]\n"},
		{name: "image platform validation", args: append(append([]string(nil), global...), "image", "pull", "example.invalid/demo", "--platform", "windows/amd64"), wantCode: 5, stderrContains: "INVALID_ARGUMENT"},
		{name: "create usage", args: append(append([]string(nil), global...), "create"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "create resource validation", args: append(append([]string(nil), global...), "create", "demo", "--name", "box", "--cpus", "0"), wantCode: 5, stderrContains: "--cpus"},
		{name: "ps usage", args: append(append([]string(nil), global...), "ps", "unexpected"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "ps output validation", args: append(append([]string(nil), global...), "ps", "--json", "--quiet"), wantCode: 5, stderrContains: "INVALID_ARGUMENT"},
		{name: "inspect missing", args: append(append([]string(nil), global...), "inspect", "missing"), wantCode: 3, stderrContains: "NOT_FOUND"},
		{name: "inspect flag validation", args: append(append([]string(nil), global...), "inspect", "missing", "--json"), wantCode: 2, stderrContains: "unknown flag"},
		{name: "logs missing", args: append(append([]string(nil), global...), "logs", "missing"), wantCode: 3, stderrContains: "NOT_FOUND"},
		{name: "logs usage", args: append(append([]string(nil), global...), "logs"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "start missing", args: append(append([]string(nil), global...), "start", "missing"), wantCode: 3, stderrContains: `Start "missing" failed`},
		{name: "start usage", args: append(append([]string(nil), global...), "start"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "stop missing", args: append(append([]string(nil), global...), "stop", "missing"), wantCode: 3, stderrContains: `Stop "missing" failed`},
		{name: "stop usage", args: append(append([]string(nil), global...), "stop"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "console usage", args: append(append([]string(nil), global...), "console"), wantCode: 2, stderrContains: "kumabox:"},
		{name: "console escape validation", args: append(append([]string(nil), global...), "console", "box", "--escape-char", "^?"), wantCode: 5, stderrContains: "--escape-char"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runBinary(t, binary, test.args, nil)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", code, test.wantCode, stdout, stderr)
			}
			if test.wantStdout != "" && stdout != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout, test.wantStdout)
			}
			if test.stdoutContains != "" && !strings.Contains(stdout, test.stdoutContains) {
				t.Fatalf("stdout = %q, want substring %q", stdout, test.stdoutContains)
			}
			if test.stderrContains != "" && !strings.Contains(stderr, test.stderrContains) {
				t.Fatalf("stderr = %q, want substring %q", stderr, test.stderrContains)
			}
			if test.wantCode == 0 && stderr != "" {
				t.Fatalf("successful command stderr = %q", stderr)
			}
			if test.wantCode != 0 && stdout != "" {
				t.Fatalf("failed command stdout = %q", stdout)
			}
			if strings.ContainsAny(stderr, "\r\x1b") {
				t.Fatalf("non-terminal stderr contains terminal controls: %q", stderr)
			}
		})
	}
}

func TestMainBinaryPropagatesSignalCancellation(t *testing.T) {
	binary := buildBinary(t)
	directory := nodeTempDir(t)
	marker := filepath.Join(directory, "ready")
	checker := filepath.Join(directory, "kumabox-check")
	script := []byte("#!/bin/sh\n: > \"$KUMABOX_TEST_READY\"\nexec sleep 30\n")
	if err := os.WriteFile(checker, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUMABOX_TEST_READY", marker)
	cfg := startBinaryTestNode(t, directory)
	command := exec.Command(binary, "--socket", cfg.SocketPath(), "doctor")
	command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "KUMABOX_TEST_READY="+marker)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	readyTimeout := time.NewTimer(15 * time.Second)
	defer readyTimeout.Stop()
	readyPoll := time.NewTicker(10 * time.Millisecond)
	defer readyPoll.Stop()

ready:
	for {
		select {
		case err := <-wait:
			t.Fatalf("doctor exited before helper readiness: %v; stderr=%q", err, stderr.String())
		case <-readyTimeout.C:
			t.Fatal("doctor helper did not become ready")
		case <-readyPoll.C:
			if _, err := os.Stat(marker); err == nil {
				break ready
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inspect doctor readiness: %v", err)
			}
		}
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wait:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("signal exit error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("kumabox did not exit after interrupt")
	}
	if stdout.Len() != 0 || strings.ContainsAny(stderr.String(), "\r\x1b") {
		t.Fatalf("signal output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "kumabox")
	command := exec.Command("go", "build", "-o", binary, ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build kumabox: %v\n%s", err, output)
	}
	return binary
}

func runBinary(t *testing.T, binary string, args, environment []string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	if environment != nil {
		command.Env = environment
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run kumabox: %v", err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func nodeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kb-main-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startBinaryTestNode(t *testing.T, base string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- daemon.Run(ctx, cfg, daemon.Options{Ready: func(string) error { close(ready); return nil }}, io.Discard)
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("node startup timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("node shutdown timed out")
		}
	})
	return cfg
}
