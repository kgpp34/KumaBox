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

func TestManagedDataDiskPrepareCloneAndCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test formatter is a POSIX shell script")
	}
	base := t.TempDir()
	paths, err := sandbox.NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	formatter := filepath.Join(base, "mkfs.ext4")
	if err := os.WriteFile(formatter, []byte("#!/bin/sh\nfor last do :; done\nprintf '\\123\\357' | dd of=\"$last\" bs=1 seek=1080 conv=notrunc 2>/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := NewExt4(paths, formatter)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	cloneID := types.SandboxID("223e4567-e89b-42d3-a456-426614174000")
	specs := []types.DataDiskSpec{{Name: "db", Size: types.MinDataDiskSize, FSType: "ext4"}, {Name: "scratch", Size: types.MinDataDiskSize, FSType: "none"}}
	if err := backend.PrepareData(t.Context(), sourceID, specs); err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckData(t.Context(), sourceID, specs); err != nil {
		t.Fatal(err)
	}
	sourceDir, err := paths.Dir(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.CloneData(t.Context(), cloneID, specs, sourceDir); err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckData(t.Context(), cloneID, specs); err != nil {
		t.Fatal(err)
	}
	if err := backend.CloneData(t.Context(), cloneID, specs, sourceDir); err == nil {
		t.Fatal("second clone overwrote an owned data disk")
	}
	cloneDB, err := paths.DataDisk(cloneID, "db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(cloneDB, types.MinDataDiskSize+1); err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckData(t.Context(), cloneID, specs); err == nil {
		t.Fatal("changed logical size was accepted")
	}
}
