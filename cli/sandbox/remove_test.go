package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/images"
	sandboxfs "github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestRemoveCommandClosesCreateAndImageReferenceLifecycle(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	seedImage(t, roots)
	installFakeMKFS(t, base)

	firstID := executeCreate(t, roots, "box")
	vmmPaths, err := vmm.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	logDir, err := vmmPaths.LogDir(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDir(logDir); err != nil {
		t.Fatal(err)
	}
	logFile, err := vmmPaths.LogFile(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte("persistent VMM output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remove := NewRemoveCommand(func() config.Config { return sandboxTestConfig(roots) })
	remove.SetArgs([]string{"box", "--json"})
	var stdout, stderr bytes.Buffer
	remove.SetOut(&stdout)
	remove.SetErr(&stderr)
	if err := remove.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var output removeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode remove output %q: %v", stdout.String(), err)
	}
	if output.ID != firstID.String() || output.Name != "box" || !strings.Contains(stdout.String(), "\n  \"id\":") {
		t.Fatalf("remove output = %+v, raw=%q", output, stdout.String())
	}
	if !strings.HasSuffix(stderr.String(), "Remove \"box\" complete\n") {
		t.Fatalf("remove progress = %q", stderr.String())
	}
	paths, err := sandboxfs.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	firstDir, err := paths.Dir(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Fatalf("removed sandbox directory still exists: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("removed sandbox log directory still exists: %v", err)
	}

	secondID := executeCreate(t, roots, "box")
	if secondID == firstID {
		t.Fatal("recreated sandbox reused immutable ID")
	}
	remove = NewRemoveCommand(func() config.Config { return sandboxTestConfig(roots) })
	remove.SetArgs([]string{secondID.String()})
	stdout.Reset()
	stderr.Reset()
	remove.SetOut(&stdout)
	remove.SetErr(&stderr)
	if err := remove.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != secondID.String()+"\n" {
		t.Fatalf("text remove output = %q", stdout.String())
	}

	state, err := core.OpenImages(t.Context(), sandboxTestConfig(roots))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := images.Remove(t.Context(), state.Paths, state.Catalog, "demo"); err != nil {
		t.Fatalf("image remains pinned after sandbox removal: %v", err)
	}
}

func executeCreate(t *testing.T, roots storage.Roots, name string) types.SandboxID {
	t.Helper()
	command := NewCreateCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{"demo", "--name", name, "--cpus", "1"})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	id, err := types.ParseSandboxID(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatalf("create output %q: %v", stdout.String(), err)
	}
	return id
}
