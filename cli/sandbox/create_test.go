package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/images"
	sandboxfs "github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

func TestParseBytes(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int64
	}{
		{"512MiB", 512 << 20},
		{"10GiB", 10 << 30},
		{"1024", 1024},
		{"1TiB", 1 << 40},
	} {
		got, err := parseBytes(test.input)
		if err != nil || got != test.want {
			t.Fatalf("parseBytes(%q) = %d, %v; want %d", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"", "-1GiB", "1GB", "1.5GiB", "0"} {
		if _, err := parseBytes(input); err == nil {
			t.Fatalf("parseBytes(%q) succeeded", input)
		}
	}
}

func TestWriteResultUsesFullIDAndIndentedJSON(t *testing.T) {
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Sandbox{
		ID:          types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		Config:      types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		ImageDigest: digest, VMM: types.VMMCloudHypervisor, State: types.SandboxStateCreated, Generation: 2,
		CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 15, 10, 0, 1, 0, time.UTC),
	}
	var text bytes.Buffer
	if err := writeSandboxResult(&text, record, false); err != nil {
		t.Fatal(err)
	}
	if text.String() != record.ID.String()+"\n" {
		t.Fatalf("text result = %q", text.String())
	}
	var jsonOut bytes.Buffer
	if err := writeSandboxResult(&jsonOut, record, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut.String(), "\n  \"id\":") || !strings.Contains(jsonOut.String(), "\"state\": \"created\"") || !strings.HasSuffix(jsonOut.String(), "\n") {
		t.Fatalf("JSON result = %q", jsonOut.String())
	}
}

func TestCreateProgressReportsCommittedOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	progress, err := newTestProgress(&stderr)
	if err != nil {
		t.Fatal(err)
	}
	progress.committed = true
	failure := errors.New("stdout closed")
	if err := progress.Finish(failure); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stderr.String(), "Create \"box\" committed with errors\n") {
		t.Fatalf("progress = %q", stderr.String())
	}
}

func TestCreateCommandPersistsCreatedSandboxAndFinalCOW(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	command := NewCreateCommand(func() storage.Roots { return roots })
	command.SetArgs([]string{"demo", "--name", "box", "--cpus", "1", "--json"})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var output sandboxOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output %q: %v", stdout.String(), err)
	}
	if output.Name != "box" || output.State != "created" || output.Generation != 2 || output.ImageDigest == "" || output.UpdatedAt.IsZero() {
		t.Fatalf("create output = %+v", output)
	}
	id, err := types.ParseSandboxID(output.ID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := sandboxfs.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	cow, err := paths.COW(id)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(cow); err != nil || info.Size() != types.DefaultSandboxStorage {
		t.Fatalf("COW stat = %+v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(roots.Data, "staging", "sandboxes")); !os.IsNotExist(err) {
		t.Fatalf("sandbox staging directory exists: %v", err)
	}
	if !strings.HasSuffix(stderr.String(), "Create \"box\" complete\n") {
		t.Fatalf("progress = %q", stderr.String())
	}
}

func seedImage(t *testing.T, roots storage.Roots) {
	t.Helper()
	state, err := core.OpenImages(t.Context(), roots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	source := digestOf(t, []byte("source"))
	erofsData := []byte("erofs")
	erofsDigest := digestOf(t, erofsData)
	kernelData, initrdData := []byte("kernel"), []byte("initrd")
	layer := types.Layer{
		SourceDigest: source, EROFSDigest: erofsDigest, Size: int64(len(erofsData)),
		BootFiles: []types.BootFile{
			{Name: "vmlinuz", Digest: digestOf(t, kernelData), Size: int64(len(kernelData))},
			{Name: "initrd.img", Digest: digestOf(t, initrdData), Size: int64(len(initrdData))},
		},
	}
	if err := storage.EnsureDir(state.Paths.BootDir(source)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Paths.EROFS(source), erofsData, 0o640); err != nil {
		t.Fatal(err)
	}
	for index, file := range layer.BootFiles {
		path, err := state.Paths.BootFile(source, file.Name)
		if err != nil {
			t.Fatal(err)
		}
		data := [][]byte{kernelData, initrdData}[index]
		if err := os.WriteFile(path, data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	boot, err := images.SelectBoot([]types.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	manifest := digestOf(t, []byte("manifest"))
	if err := state.Catalog.CommitImport(t.Context(), images.ImportCommit{
		Name: "demo", Manifest: types.Manifest{Digest: manifest, Platform: types.Platform{OS: "linux", Architecture: "amd64"}, Layers: []types.Descriptor{{Digest: source, Size: 6}}},
		Layers: []types.Layer{layer}, Boot: boot, Size: layer.Size, Created: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func digestOf(t *testing.T, data []byte) types.Digest {
	t.Helper()
	sum := sha256.Sum256(data)
	digest, err := types.ParseDigest(fmt.Sprintf("sha256:%x", sum))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func installFakeMKFS(t *testing.T, base string) {
	t.Helper()
	binDir := filepath.Join(base, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	formatter := filepath.Join(binDir, "mkfs.ext4")
	script := []byte("#!/bin/sh\nfor last do :; done\nprintf '\\123\\357' | dd of=\"$last\" bs=1 seek=1080 conv=notrunc 2>/dev/null\n")
	if err := os.WriteFile(formatter, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newTestProgress(writer *bytes.Buffer) (*sandboxProgress, error) {
	progress := &sandboxProgress{
		writer: writer, operation: "create sandbox", label: `Create "box"`, status: "preparing sandbox", recovery: "inspect the sandbox state",
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := progress.render(); err != nil {
		return nil, err
	}
	close(progress.done)
	return progress, nil
}
