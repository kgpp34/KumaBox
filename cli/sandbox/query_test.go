package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestSandboxTableHasHeadersAndActionableID(t *testing.T) {
	record := testSandboxRecord(t)
	var output bytes.Buffer
	if err := writeSandboxTable(&output, []types.Sandbox{record}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"SANDBOX ID", "NAME", "IMAGE ID", "STATE", "CPUS", "MEMORY", "STORAGE", "CREATED",
		record.ID.String(), "box", record.ImageDigest.Hex()[:12], "created", "1.0GiB", "10.0GiB", "2026-09-16T02:00:00Z",
	} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("table missing %q:\n%s", value, output.String())
		}
	}
	if strings.ContainsAny(output.String(), "\t\x1b") || strings.Contains(output.String(), record.ImageDigest.String()) {
		t.Fatalf("table contains tabs, terminal controls, or a full image digest: %q", output.String())
	}
}

func TestEmptySandboxOutputsRemainScriptFriendly(t *testing.T) {
	var table bytes.Buffer
	if err := writeSandboxTable(&table, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(table.String(), "\n") != 1 || !strings.Contains(table.String(), "SANDBOX ID") {
		t.Fatalf("empty table = %q", table.String())
	}
	var jsonOutput bytes.Buffer
	if err := writeSandboxListJSON(&jsonOutput, nil); err != nil {
		t.Fatal(err)
	}
	if jsonOutput.String() != "[]\n" {
		t.Fatalf("empty JSON = %q", jsonOutput.String())
	}
	var quiet bytes.Buffer
	if err := writeSandboxIDs(&quiet, nil); err != nil {
		t.Fatal(err)
	}
	if quiet.Len() != 0 {
		t.Fatalf("empty quiet output = %q", quiet.String())
	}
}

func TestInspectCommandAlwaysReturnsIndentedJSONByNameOrID(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	id := executeCreate(t, roots, "box")

	byName := executeInspect(t, roots, "box")
	byID := executeInspect(t, roots, id.String())
	if byName != byID || !strings.Contains(byName, "\n  \"id\"") {
		t.Fatalf("inspect outputs are not identical indented JSON:\nname=%s\nid=%s", byName, byID)
	}
	var output sandboxOutput
	if err := json.Unmarshal([]byte(byName), &output); err != nil {
		t.Fatalf("decode inspect JSON %q: %v", byName, err)
	}
	if output.ID != id.String() || output.Name != "box" || output.State != "created" || output.Failure != nil {
		t.Fatalf("inspect = %+v", output)
	}
	if output.ImageDigest == "" || output.CPUs != 1 || output.Memory != types.DefaultSandboxMemory || output.Storage != types.DefaultSandboxStorage {
		t.Fatalf("inspect omitted identity or resources: %+v", output)
	}
}

func TestSandboxJSONIncludesRetainedFailure(t *testing.T) {
	record := testSandboxRecord(t)
	record.State = types.SandboxStateError
	record.Failure = &types.SandboxFailure{Phase: "disk", Message: "mkfs failed"}
	var output bytes.Buffer
	if err := writeSandboxJSON(&output, record); err != nil {
		t.Fatal(err)
	}
	var decoded sandboxOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Failure == nil || decoded.Failure.Phase != "disk" || decoded.Failure.Message != "mkfs failed" {
		t.Fatalf("failure = %+v", decoded.Failure)
	}
}

func TestListCommandShowsCreatedOnlyWithAllAndTracksRemoval(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	id := executeCreate(t, roots, "box")

	if output := executeList(t, roots); strings.Contains(output, id.String()) || !strings.Contains(output, "SANDBOX ID") {
		t.Fatalf("default ps output = %q", output)
	}
	jsonOutput := executeList(t, roots, "--all", "--json")
	var records []sandboxOutput
	if err := json.Unmarshal([]byte(jsonOutput), &records); err != nil {
		t.Fatalf("decode ps JSON %q: %v", jsonOutput, err)
	}
	if len(records) != 1 || records[0].ID != id.String() || records[0].Name != "box" || records[0].State != "created" {
		t.Fatalf("ps --all --json = %+v", records)
	}
	if output := executeList(t, roots, "-a", "--quiet"); output != id.String()+"\n" {
		t.Fatalf("ps --all --quiet = %q", output)
	}
	if output := executeList(t, roots, "-a"); !strings.Contains(output, id.String()) || !strings.Contains(output, "box") {
		t.Fatalf("ps --all table = %q", output)
	}

	remove := NewRemoveCommand(func() config.Config { return sandboxTestConfig(roots) })
	remove.SetArgs([]string{id.String()})
	remove.SetOut(&bytes.Buffer{})
	remove.SetErr(&bytes.Buffer{})
	if err := remove.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if output := executeList(t, roots, "--all", "--json"); output != "[]\n" {
		t.Fatalf("ps after rm = %q", output)
	}
}

func TestListCommandRejectsJSONWithQuiet(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	command := NewListCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{"--json", "--quiet"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("ps accepted --json with --quiet")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("ps error code = %q, %v", code, err)
	}
}

func TestLogsCommandStreamsTailByName(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	id := executeCreate(t, roots, "box")
	paths, err := vmm.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	logDir, err := paths.LogDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDir(logDir); err != nil {
		t.Fatal(err)
	}
	logFile, err := paths.LogFile(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := NewLogsCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{"--tail", "1", "box"})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if output.String() != "second\n" {
		t.Fatalf("logs output = %q", output.String())
	}

	command = NewLogsCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{"box", "--tail", "-1"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("logs accepted negative --tail")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("logs error = %v", err)
	}
}

type sandboxFailingOutput struct{ err error }

func (writer sandboxFailingOutput) Write([]byte) (int, error) { return 0, writer.err }

func TestSandboxListOutputPreservesWriteErrors(t *testing.T) {
	failure := errors.New("output closed")
	for name, write := range map[string]func() error{
		"inspect": func() error { return writeSandboxJSON(sandboxFailingOutput{failure}, testSandboxRecord(t)) },
		"table":   func() error { return writeSandboxTable(sandboxFailingOutput{failure}, nil) },
		"json":    func() error { return writeSandboxListJSON(sandboxFailingOutput{failure}, nil) },
		"quiet": func() error {
			return writeSandboxIDs(sandboxFailingOutput{failure}, []types.Sandbox{testSandboxRecord(t)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, failure) {
				t.Fatalf("write error = %v", err)
			}
		})
	}
}

func executeInspect(t *testing.T, roots storage.Roots, reference string) string {
	t.Helper()
	command := NewInspectCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{reference})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func executeList(t *testing.T, roots storage.Roots, args ...string) string {
	t.Helper()
	command := NewListCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs(args)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func testSandboxRecord(t *testing.T) types.Sandbox {
	t.Helper()
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 16, 10, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	return types.Sandbox{
		ID:          types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		Config:      types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		ImageDigest: digest, VMM: types.VMMCloudHypervisor, State: types.SandboxStateCreated, Generation: 2,
		CreatedAt: created, UpdatedAt: created.Add(time.Second),
	}
}
