package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func archiveTestRecord() types.Snapshot {
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		panic(err)
	}
	return types.Snapshot{
		ID:   types.SnapshotID("223e4567-e89b-42d3-a456-426614174000"),
		Name: "warm-base", SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		SourceGeneration: 4,
		ImageDigest:      digest,
		VMM:              types.VMMCloudHypervisor,
		Config:           types.SandboxConfig{Name: "source", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt:        time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
	}
}

func TestArchiveRoundTripAndRejectsCorruption(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "tar", true: "gzip"}[compressed], func(t *testing.T) {
			source := t.TempDir()
			for name, content := range map[string]string{
				"config.json": "{\"cpus\":2}", "state.json": "{\"version\":1}",
				"memory-range-0": "memory-payload", "cow.raw": "disk-payload",
			} {
				if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var archive bytes.Buffer
			original := archiveTestRecord()
			if err := WriteArchive(t.Context(), &archive, source, original, compressed); err != nil {
				t.Fatal(err)
			}
			destination := t.TempDir()
			restored, err := ReadArchive(t.Context(), bytes.NewReader(archive.Bytes()), destination)
			if err != nil || restored.ID != original.ID || restored.ImageDigest != original.ImageDigest {
				t.Fatalf("ReadArchive() = %+v, %v", restored, err)
			}
			memory, err := os.ReadFile(filepath.Join(destination, "memory-range-0"))
			if err != nil || string(memory) != "memory-payload" {
				t.Fatalf("restored memory = %q, %v", memory, err)
			}
			cut := archive.Bytes()[:archive.Len()/2]
			if _, err := ReadArchive(t.Context(), bytes.NewReader(cut), t.TempDir()); err == nil {
				t.Fatal("truncated archive was accepted")
			}
			if !compressed {
				corrupt := bytes.Replace(archive.Bytes(), []byte("memory-payload"), []byte("memory-PAYLOAD"), 1)
				if _, err := ReadArchive(t.Context(), bytes.NewReader(corrupt), t.TempDir()); err == nil {
					t.Fatal("modified archive payload was accepted")
				}
			}
			if compressed {
				broken := bytes.Clone(archive.Bytes())
				broken[len(broken)-5] ^= 0xff
				if _, err := ReadArchive(t.Context(), bytes.NewReader(broken), t.TempDir()); err == nil {
					t.Fatal("damaged gzip trailer was accepted")
				}
			}
		})
	}
}

func TestReadArchiveRejectsTraversalBeforeWriting(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(t.Context(), &archive, stage); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path traversal wrote outside stage: %v", err)
	}
}
