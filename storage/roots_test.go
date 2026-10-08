package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootsRejectOverlap(t *testing.T) {
	root := t.TempDir()
	_, err := (Roots{Data: root, Run: filepath.Join(root, "run"), Log: filepath.Join(root, "log")}).Validate()
	if err == nil {
		t.Fatal("Validate accepted overlapping roots")
	}
}

func TestJoinRejectsEscape(t *testing.T) {
	if _, err := Join(t.TempDir(), "..", "escape"); err == nil {
		t.Fatal("Join accepted parent traversal")
	}
}

func TestPathsRejectSymlinkParents(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "images")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(filepath.Join(base, "images", "layers")); err == nil {
		t.Fatal("followed symlink parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "layers")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote outside managed path: %v", err)
	}
	if _, err := (Roots{Data: filepath.Join(base, "images", "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}).Validate(); err == nil {
		t.Fatal("accepted symlink root parent")
	}
}
