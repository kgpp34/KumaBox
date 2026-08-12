//go:build linux

package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var machineIDPattern = regexp.MustCompile(`^[0-9a-f]{32}\n$`)

func TestRandomMachineIDIsCanonicalAndUnique(t *testing.T) {
	first, err := randomMachineID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomMachineID()
	if err != nil {
		t.Fatal(err)
	}
	if !machineIDPattern.MatchString(first) || !machineIDPattern.MatchString(second) || first == second {
		t.Fatalf("machine IDs = %q and %q", first, second)
	}
}

func TestDropStaleDBusMachineID(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "machine-id")
		if err := os.WriteFile(path, []byte("old\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := dropStaleDBusMachineID(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("regular file remains: %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("id\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "machine-id")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := dropStaleDBusMachineID(link); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was not preserved: info=%v error=%v", info, err)
		}
	})
}
