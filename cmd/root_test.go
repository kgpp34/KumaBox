package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoctorForwardsArgumentsAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}

	dir := t.TempDir()
	checker := filepath.Join(dir, "kumabox-check")
	contents := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nexit 1\n")
	if err := os.WriteFile(checker, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout bytes.Buffer
	err := Execute(context.Background(), []string{
		"doctor", "--fix", "--upgrade", "--subnet=10.89.0.0/16",
	}, &stdout, &bytes.Buffer{})
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode() = %d, want 1; err = %v", got, err)
	}
	if !Silent(err) {
		t.Fatal("doctor process error must be silent")
	}
	if got, want := stdout.String(), "--fix\n--upgrade\n--subnet=10.89.0.0/16\n"; got != want {
		t.Fatalf("forwarded arguments = %q, want %q", got, want)
	}
}

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	err := Execute(context.Background(), []string{"version"}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "kumabox ") {
		t.Fatalf("version output = %q", stdout.String())
	}
}
