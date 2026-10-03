package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloneFileCreatesIndependentWritableCopy(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.raw")
	target := filepath.Join(directory, "target.raw")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(2 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("source"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CloneFile(target, source); err != nil {
		t.Fatal(err)
	}
	copyFile, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copyFile.WriteAt([]byte("target"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := copyFile.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close() //nolint:errcheck // read-only test handle
	actual := make([]byte, len("source"))
	if _, err := original.ReadAt(actual, 1<<20); err != nil || string(actual) != "source" {
		t.Fatalf("source changed to %q: %v", actual, err)
	}
}
