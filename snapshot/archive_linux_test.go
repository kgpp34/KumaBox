//go:build linux

package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivePreservesSparseCOWWithoutSendingLogicalZeros(t *testing.T) {
	source := t.TempDir()
	for name, content := range map[string]string{
		"config.json": "{}", "state.json": "{}", "memory-range-0": "memory",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Create(filepath.Join(source, "cow.raw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(archiveTestRecord().Config.Storage); err != nil {
		t.Fatal(err)
	}
	if _, sparse, err := scanSparse(file, archiveTestRecord().Config.Storage); err != nil {
		t.Fatal(err)
	} else if !sparse {
		t.Skip("test filesystem does not expose sparse extents")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, compress := range []bool{false, true} {
		var archive bytes.Buffer
		if err := WriteArchive(t.Context(), &archive, source, archiveTestRecord(), compress); err != nil {
			t.Fatal(err)
		}
		if archive.Len() > 1<<20 {
			t.Fatalf("sparse archive unexpectedly uses %d bytes (gzip=%t)", archive.Len(), compress)
		}
		destination := t.TempDir()
		if _, err := ReadArchive(t.Context(), &archive, destination); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(destination, "cow.raw"))
		if err != nil || info.Size() != archiveTestRecord().Config.Storage {
			t.Fatalf("restored sparse COW = %v, %v", info, err)
		}
	}
}
