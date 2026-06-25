package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionJSONCommand(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var payload map[string]string
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] == "" {
		t.Fatal("version must not be empty")
	}
}

func TestDoctorInitializesConfiguredDirectories(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
		"doctor",
		"--json",
	})

	_ = cmd.Execute()

	for _, path := range []string{rootDir, runDir, logDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
	}

	var payload struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	for _, check := range payload.Checks {
		if check.Name == "paths" && check.Status == "pass" {
			return
		}
	}
	t.Fatal("doctor output did not include passing paths check")
}
