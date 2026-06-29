package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		"--cloud-hypervisor-bin", "/custom/bin/cloud-hypervisor",
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
	rawConfig, err := os.ReadFile(created.Config)
	if err != nil {
		t.Fatal(err)
	}
	var renderedConfig struct {
		Binary string `json:"binary"`
	}
	if err := json.Unmarshal(rawConfig, &renderedConfig); err != nil {
		t.Fatal(err)
	}
	if renderedConfig.Binary != "/custom/bin/cloud-hypervisor" {
		t.Fatalf("rendered binary = %s", renderedConfig.Binary)
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

func TestCreateFirmwareBootCommand(t *testing.T) {
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
		"--name", "uefi",
		"--root-disk", "fixtures/ubuntu.img",
		"--firmware", "fixtures/CLOUDHV.fd",
	})
	var out bytes.Buffer
	create.SetOut(&out)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		Config   string `json:"config"`
		Firmware string `json:"firmware"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Firmware == "" {
		t.Fatal("expected firmware in create output")
	}

	rawConfig, err := os.ReadFile(created.Config)
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Firmware *struct {
			Path string `json:"path"`
		} `json:"firmware"`
		Kernel any `json:"kernel"`
	}
	if err := json.Unmarshal(rawConfig, &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Firmware == nil || rendered.Firmware.Path == "" {
		t.Fatalf("rendered firmware = %+v", rendered.Firmware)
	}
	if rendered.Kernel != nil {
		t.Fatalf("expected no direct kernel payload: %+v", rendered.Kernel)
	}
}

func TestLogsCommandTailsVMLogs(t *testing.T) {
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
		"--name", "loggy",
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
		LogDir string `json:"logDir"`
	}
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(created.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.LogDir, "cloud-hypervisor.stdout.log"), []byte("line-1\nline-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.LogDir, "cloud-hypervisor.stderr.log"), []byte("err-1\nerr-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := NewRootCommand()
	logs.SetArgs([]string{"--root-dir", rootDir, "logs", "loggy", "--tail", "1"})
	var logsOut bytes.Buffer
	logs.SetOut(&logsOut)
	if err := logs.Execute(); err != nil {
		t.Fatal(err)
	}

	got := logsOut.String()
	if !strings.Contains(got, "==> cloud-hypervisor.stdout.log <==") {
		t.Fatalf("logs output missing stdout header: %s", got)
	}
	if strings.Contains(got, "line-1") || !strings.Contains(got, "line-2") {
		t.Fatalf("logs output did not tail stdout: %s", got)
	}
	if strings.Contains(got, "err-1") || !strings.Contains(got, "err-2") {
		t.Fatalf("logs output did not tail stderr: %s", got)
	}
}
