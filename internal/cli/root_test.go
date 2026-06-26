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

func TestCreateInspectAndPSCommands(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
		"create",
		"--name", "p0-store",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	})
	var createOut bytes.Buffer
	create.SetOut(&createOut)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Config string `json:"config"`
	}
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "p0-store" || created.State != "created" {
		t.Fatalf("unexpected create output: %+v", created)
	}
	if created.Config == "" {
		t.Fatal("expected rendered backend config path")
	}
	if _, err := os.Stat(created.Config); err != nil {
		t.Fatal(err)
	}

	inspect := NewRootCommand()
	inspect.SetArgs([]string{
		"--root-dir", rootDir,
		"inspect", "p0-store", "--json",
	})
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}

	var inspected struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(inspectOut.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != created.ID {
		t.Fatalf("inspect id = %s, want %s", inspected.ID, created.ID)
	}

	ps := NewRootCommand()
	ps.SetArgs([]string{"--root-dir", rootDir, "ps", "--json"})
	var psOut bytes.Buffer
	ps.SetOut(&psOut)
	if err := ps.Execute(); err != nil {
		t.Fatal(err)
	}

	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(psOut.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != created.ID {
		t.Fatalf("ps records = %+v", records)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--root-dir", filepath.Join(dir, "data"),
		"--run-dir", filepath.Join(dir, "run"),
		"--log-dir", filepath.Join(dir, "log"),
		"create",
		"--name", "duplicate",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	}

	first := NewRootCommand()
	first.SetArgs(args)
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}

	second := NewRootCommand()
	second.SetArgs(args)
	if err := second.Execute(); err == nil {
		t.Fatal("expected duplicate name error")
	}
}
