package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestClassifyImageSource(t *testing.T) {
	local := filepath.Join(t.TempDir(), "image.qcow2")
	if err := os.WriteFile(local, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		source string
		want   imageSourceKind
	}{
		{name: "local", source: local, want: imageSourceLocal},
		{name: "HTTP", source: "https://example.com/image.qcow2", want: imageSourceHTTP},
		{name: "OCI", source: "ubuntu:24.04", want: imageSourceOCI},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyImageSource(test.source)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("kind = %q, want %q", got, test.want)
			}
		})
	}
	if _, err := classifyImageSource(filepath.Join(t.TempDir(), "missing.qcow2")); err == nil {
		t.Fatal("expected missing explicit path error")
	}
}

func TestImageAddImportsLocalCloudImage(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.img")
	firmware := filepath.Join(directory, "firmware.fd")
	for path, content := range map[string]string{source: "image", firmware: "firmware"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := newTestRootCommand(filepath.Join(directory, "data"))
	command.SetArgs([]string{
		"image", "add", source, "--name", "local-image", "--firmware", firmware,
		"--qemu-img", fakeQemuImgForCLI(t, directory, "qcow2", 4096, 5),
	})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var record imagestore.ImageRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Name != "local-image" || record.Source.Type != "local-file" {
		t.Fatalf("image record = %+v", record)
	}
}

func TestDebugLaunchIsSideEffectFree(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "data")
	run := filepath.Join(directory, "run")
	logDirectory := filepath.Join(directory, "log")
	image, err := imagestore.New(root).Create(imagestore.CreateRequest{
		Name: "debug-image", Source: imagestore.Source{Type: "test", URI: "source.qcow2"},
		RootDisk: imagestore.RootDisk{
			Path: "/images/source.qcow2", Format: vmstore.FormatQCOW2,
			VirtualSizeBytes: 1 << 20, SHA256: strings.Repeat("a", 64),
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "/firmware.fd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := newTestRootCommand(root, run, logDirectory)
	command.SetArgs([]string{"debug", "launch", image.Name, "--json", "--memory", "256M"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		SchemaVersion string `json:"schemaVersion"`
		DryRun        bool   `json:"dryRun"`
		VM            struct {
			ID          string   `json:"id"`
			MemoryBytes int64    `json:"memoryBytes"`
			Networks    []string `json:"networks"`
		} `json:"vm"`
		Launch struct {
			Args []string `json:"args"`
		} `json:"launch"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != "kumabox.debug.launch.v1" || !result.DryRun || result.VM.ID != "kb_preview" {
		t.Fatalf("debug result = %+v", result)
	}
	if result.VM.MemoryBytes != 256<<20 || len(result.VM.Networks) != 1 || result.VM.Networks[0] != "none" {
		t.Fatalf("preview VM = %+v", result.VM)
	}
	records, err := vmstore.New(root).List()
	if err != nil || len(records) != 0 {
		t.Fatalf("persisted VMs = %+v, err = %v", records, err)
	}
	if _, err := os.Stat(run); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory stat error = %v", err)
	}
}

func TestSnapshotDirectoryCLIExportImport(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "data")
	store := snapshot.NewStore(root)
	build, err := store.Reserve(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	disk := []byte("snapshot-directory")
	diskPath := filepath.Join(build.Record().StagingDir, "disks", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, disk, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(disk)
	manifest := snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v1", ID: build.Record().ID, Name: "source",
		Type: "disk", Consistency: "stopped-disk",
		Disks: []snapshot.DiskManifest{{
			ID: "root", Role: "cow", Path: "disks/root.qcow2", Format: "qcow2",
			VirtualSizeBytes: int64(len(disk)), AllocatedSizeBytes: int64(len(disk)),
			SHA256: hex.EncodeToString(digest[:]), CopyStrategy: "stream",
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build.Record().StagingDir, snapshot.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := build.Finalize(int64(len(disk))); err != nil {
		t.Fatal(err)
	}

	exported := filepath.Join(directory, "exported")
	exportCommand := newTestRootCommand(root)
	exportCommand.SetArgs([]string{"snapshot", "export", "source", "--to-dir", exported})
	if err := exportCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	importCommand := newTestRootCommand(root)
	importCommand.SetArgs([]string{
		"--qemu-img-bin", fakeImportQEMUImgForCLI(t, directory),
		"snapshot", "import", "--from-dir", exported, "--name", "imported",
	})
	if err := importCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect("imported"); err != nil {
		t.Fatal(err)
	}
}

func fakeImportQEMUImgForCLI(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "qemu-img-import")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' '{\"format\":\"qcow2\",\"virtual-size\":18}'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
