package doctor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/daemon"
	"github.com/kumabox/kumabox/storage"
)

func TestCommandLetsScriptOwnFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}

	dir, err := os.MkdirTemp("/tmp", "kb-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	checker := filepath.Join(dir, checkerName)
	contents := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	if err := os.WriteFile(checker, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(dir, "data"), Run: filepath.Join(dir, "run"), Log: filepath.Join(dir, "log")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- daemon.Run(ctx, cfg, daemon.Options{Ready: func(string) error { close(ready); return nil }}, io.Discard)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("node startup timed out")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	command := NewCommand(func() config.Config { return cfg })
	command.SetArgs([]string{"--help", "--future-script-flag=value"})
	command.SetContext(context.Background())
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "--help\n--future-script-flag=value\n"; got != want {
		t.Fatalf("forwarded arguments = %q, want %q", got, want)
	}
}
