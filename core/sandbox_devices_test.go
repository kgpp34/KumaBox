package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/storage"
)

func TestResolveExternalDiskRejectsManagedPathsAfterSymlinks(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "data")
	external := filepath.Join(root, "external")
	if err := os.Mkdir(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(managed, "owned.raw")
	outside := filepath.Join(external, "volume.raw")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("raw"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(external, "alias.raw")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	roots := storage.Roots{Data: managed, Run: filepath.Join(root, "run"), Log: filepath.Join(root, "log")}
	if _, err := resolveExternalDisk(inside, roots); err == nil {
		t.Fatal("managed disk was accepted")
	}
	if _, err := resolveExternalDisk(alias, roots); err == nil {
		t.Fatal("symlink to managed disk was accepted")
	}
	expected, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveExternalDisk(outside, roots); err != nil || got != expected {
		t.Fatalf("external disk = %q, %v", got, err)
	}
}

func TestNormalizePCIPathRejectsOtherHostPaths(t *testing.T) {
	for input, expected := range map[string]string{
		"01:00.0":                           "/sys/bus/pci/devices/0000:01:00.0",
		"0000:03:1a.2":                      "/sys/bus/pci/devices/0000:03:1a.2",
		"/sys/bus/pci/devices/0000:03:1a.2": "/sys/bus/pci/devices/0000:03:1a.2",
	} {
		got, err := normalizePCIPath(input)
		if err != nil || got != expected {
			t.Fatalf("normalize %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"/etc/passwd", "/sys/bus/pci/devices/../../etc/passwd", "01:00.8"} {
		if _, err := normalizePCIPath(input); err == nil {
			t.Fatalf("unsafe PCI path %q was accepted", input)
		}
	}
}
