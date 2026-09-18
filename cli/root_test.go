package cli

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

func TestImageAndUsageExitCodes(t *testing.T) {
	base := t.TempDir()
	flags := []string{"--root-dir", filepath.Join(base, "data"), "--run-dir", filepath.Join(base, "run"), "--log-dir", filepath.Join(base, "log")}
	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{"unknown command", []string{"unknown"}, 2},
		{"unknown image command", []string{"image", "unknown"}, 2},
		{"missing image argument", []string{"image", "inspect"}, 2},
		{"missing create image", []string{"create", "--name", "box"}, 2},
		{"missing console sandbox", []string{"console"}, 2},
		{"missing exec command", []string{"exec", "box"}, 2},
		{"missing inspect sandbox", []string{"inspect"}, 2},
		{"missing remove sandbox", []string{"rm"}, 2},
		{"missing start sandbox", []string{"start"}, 2},
		{"missing stop sandbox", []string{"stop"}, 2},
		{"unexpected ps argument", []string{"ps", "box"}, 2},
		{"unsupported inspect flag", []string{"inspect", "box", "--json"}, 2},
		{"unknown flag", []string{"image", "ls", "--wrong"}, 2},
		{"unsupported platform", []string{"image", "pull", "example.com/image", "--platform", "windows/amd64"}, 5},
		{"incompatible ps output", []string{"ps", "--json", "--quiet"}, 5},
		{"missing image", []string{"image", "inspect", "missing"}, 3},
		{"missing inspected sandbox", []string{"inspect", "missing"}, 3},
		{"missing sandbox", []string{"rm", "missing"}, 3},
		{"empty list", []string{"image", "ls", "--json"}, 0},
		{"empty ps", []string{"ps", "--all", "--json"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string(nil), flags...), test.args...)
			err := Execute(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{})
			if got := ExitCode(err); got != test.code {
				t.Fatalf("exit = %d, want %d, error %v", got, test.code, err)
			}
		})
	}
}
