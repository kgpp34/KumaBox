package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/client"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/core"
	"github.com/kumabox/kumabox/internal/images"
	sandboxfs "github.com/kumabox/kumabox/internal/sandbox"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

func TestRemoveCommandClosesCreateAndImageReferenceLifecycle(t *testing.T) {
	base := shortTempDir(t)
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
	remove := NewRemoveCommand(func() config.Config { return sandboxTestConfig(t, roots) })
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
	remove = NewRemoveCommand(func() config.Config { return sandboxTestConfig(t, roots) })
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

	state, err := core.OpenImages(t.Context(), sandboxTestConfig(t, roots))
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

func TestRemoveBatchReportsSuccessesEvenWhenOneReferenceFails(t *testing.T) {
	for _, test := range []struct {
		name  string
		force bool
	}{
		{name: "normal"},
		{name: "forced", force: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := shortTempDir(t)
			roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
			seedImage(t, roots)
			installFakeMKFS(t, base)
			first := executeCreate(t, roots, "first")
			second := executeCreate(t, roots, "second")
			command := NewRemoveCommand(func() config.Config { return sandboxTestConfig(t, roots) })
			args := []string{"first", "missing", "second", "--json"}
			if test.force {
				args = append(args, "--force")
			}
			command.SetArgs(args)
			command.SilenceUsage = true
			command.SilenceErrors = true
			var stdout bytes.Buffer
			command.SetOut(&stdout)
			command.SetErr(&bytes.Buffer{})
			if err := command.ExecuteContext(t.Context()); err == nil {
				t.Fatal("batch removal hid the missing reference")
			}
			var removed []removeOutput
			if err := json.Unmarshal(stdout.Bytes(), &removed); err != nil {
				t.Fatalf("batch output %q: %v", stdout.String(), err)
			}
			if len(removed) != 2 || removed[0].ID != first.String() || removed[1].ID != second.String() {
				t.Fatalf("batch output = %+v", removed)
			}
			if output := executeList(t, roots, "--all", "--json"); output != "[]\n" {
				t.Fatalf("batch left sandboxes: %q", output)
			}
		})
	}
}

func TestRemovePreservesLegacyClientAndSupportsForceShorthand(t *testing.T) {
	base := shortTempDir(t)
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	first := executeCreate(t, roots, "legacy")
	cfg := sandboxTestConfig(t, roots)
	service, err := client.OpenSandbox(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	removed, err := service.Remove(t.Context(), "legacy")
	if err != nil || removed.ID != first {
		t.Fatalf("legacy remove request = %+v, %v", removed, err)
	}
	second := executeCreate(t, roots, "forced")
	command := NewRemoveCommand(func() config.Config { return cfg })
	command.SetArgs([]string{"-f", "forced", "--json"})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var result removeOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != second.String() || result.Name != "forced" {
		t.Fatalf("forced removal = %+v", result)
	}
}

func executeCreate(t *testing.T, roots storage.Roots, name string) types.SandboxID {
	t.Helper()
	command := NewCreateCommand(func() config.Config { return sandboxTestConfig(t, roots) })
	command.SetArgs([]string{"demo", "--name", name, "--cpus", "1", "--nics", "0"})
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
