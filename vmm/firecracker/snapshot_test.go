package firecracker

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

type snapshotFixture struct {
	driver    *Driver
	meta      snapshotConfig
	directory string
}

func testSnapshot(t *testing.T) *snapshotFixture {
	t.Helper()
	base := t.TempDir()
	paths, err := vmm.NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := types.NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	vsockPath, err := paths.Vsock(id)
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotFixture{driver: &Driver{paths: paths}, directory: filepath.Join(base, "capture")}
	f.meta = snapshotConfig{
		Version: 1, SandboxID: id,
		Config: vmConfig{
			Machine: machineConfig{VCPUs: 2, MemoryMiB: 512},
			Boot:    bootSource{Kernel: filepath.Join(paths.KernelCache(), "digest", "vmlinux")},
			Vsock:   vsock{CID: vmm.VsockGuestCID, Path: vsockPath},
			Drives:  []drive{{ID: "disk0", Path: filepath.Join(base, "layer.erofs"), ReadOnly: true}, {ID: "disk1", Path: filepath.Join(base, "cow.raw")}},
		},
		Writable: []capturedDisk{{Source: filepath.Join(base, "cow.raw"), File: "cow.raw"}},
	}
	if err := os.Mkdir(f.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.meta.Config.Drives[0].Path, f.meta.Config.Drives[1].Path, filepath.Join(f.directory, "cow.raw"), filepath.Join(f.directory, snapshotStateFile)} {
		if err := os.WriteFile(path, []byte("captured"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Create(filepath.Join(f.directory, snapshotMemoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(512 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	f.writeMeta(t)
	return f
}

func (f *snapshotFixture) writeMeta(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.directory, snapshotConfigFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSnapshotRejectsIncompleteAndUnsafeArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage func(*testing.T, *snapshotFixture)
	}{
		{name: "missing memory", damage: func(t *testing.T, f *snapshotFixture) {
			if err := os.Remove(filepath.Join(f.directory, snapshotMemoryFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong memory size", damage: func(t *testing.T, f *snapshotFixture) {
			if err := os.Truncate(filepath.Join(f.directory, snapshotMemoryFile), 1); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink state", damage: func(t *testing.T, f *snapshotFixture) {
			path := filepath.Join(f.directory, snapshotStateFile)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.meta.Config.Drives[0].Path, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown version", damage: func(t *testing.T, f *snapshotFixture) { f.meta.Version = 999; f.writeMeta(t) }},
		{name: "missing version two owner", damage: func(t *testing.T, f *snapshotFixture) { f.meta.Version = 2; f.writeMeta(t) }},
		{name: "memory byte size overflow", damage: func(t *testing.T, f *snapshotFixture) {
			f.meta.Config.Machine.MemoryMiB = math.MaxInt64
			f.writeMeta(t)
		}},
		{name: "uncaptured disk", damage: func(t *testing.T, f *snapshotFixture) {
			f.meta.Config.Drives = append(f.meta.Config.Drives, drive{ID: "extra", Path: "/foreign/disk.raw"})
			f.writeMeta(t)
		}},
		{name: "escaping file", damage: func(t *testing.T, f *snapshotFixture) { f.meta.Writable[0].File = "../cow.raw"; f.writeMeta(t) }},
		{name: "duplicate device", damage: func(t *testing.T, f *snapshotFixture) { f.meta.Config.Drives[1].ID = "disk0"; f.writeMeta(t) }},
		{name: "missing COW capture", damage: func(t *testing.T, f *snapshotFixture) {
			if err := os.Remove(filepath.Join(f.directory, "cow.raw")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := testSnapshot(t)
			if err := f.driver.ValidateSnapshot(t.Context(), f.directory); err != nil {
				t.Fatal(err)
			}
			test.damage(t, f)
			if err := f.driver.ValidateSnapshot(t.Context(), f.directory); err == nil {
				t.Fatal("accepted damaged snapshot")
			}
		})
	}
}

func TestValidateSnapshotDoesNotRequireResidentFiles(t *testing.T) {
	f := testSnapshot(t)
	for _, disk := range f.meta.Config.Drives {
		if err := os.Remove(disk.Path); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.driver.ValidateSnapshot(t.Context(), f.directory); err != nil {
		t.Fatalf("portable import depended on resident files: %v", err)
	}
	if err := f.driver.ValidateRestore(t.Context(), f.directory, types.MemoryRestoreAuto); err == nil {
		t.Fatal("restore accepted absent resident disks")
	}
}
