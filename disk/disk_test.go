package disk

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

func TestExt4PreparesFinalSparsePathAndRemovesIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test formatter is a POSIX shell script")
	}
	base := t.TempDir()
	paths, err := sandbox.NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	formatter := filepath.Join(base, "mkfs.ext4")
	script := []byte("#!/bin/sh\nfor last do :; done\nprintf '\\123\\357' | dd of=\"$last\" bs=1 seek=1080 conv=notrunc 2>/dev/null\n")
	if err := os.WriteFile(formatter, script, 0o755); err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	preparer, err := NewExt4(paths, formatter)
	if err != nil {
		t.Fatal(err)
	}
	if preparer.mkfs != formatter {
		t.Fatalf("formatter = %q, want %q", preparer.mkfs, formatter)
	}
	if err := preparer.Prepare(t.Context(), id, types.MinSandboxStorage); err != nil {
		t.Fatal(err)
	}
	cow, err := paths.COW(id)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cow)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != types.MinSandboxStorage {
		t.Fatalf("COW size = %d, want %d", info.Size(), types.MinSandboxStorage)
	}
	if err := preparer.Prepare(t.Context(), id, types.MinSandboxStorage); err == nil {
		t.Fatal("second prepare replaced an owned COW")
	}
	if err := preparer.Remove(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cow); !os.IsNotExist(err) {
		t.Fatalf("COW remains after Remove: %v", err)
	}
}

func TestNewExt4RejectsMissingFormatter(t *testing.T) {
	if _, err := NewExt4(sandbox.Paths{}, ""); err == nil {
		t.Fatal("NewExt4() accepted an empty formatter")
	}
}
