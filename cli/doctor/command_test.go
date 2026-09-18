package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCommandLetsScriptOwnFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}

	dir := t.TempDir()
	checker := filepath.Join(dir, checkerName)
	contents := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	if err := os.WriteFile(checker, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	command := NewCommand()
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
