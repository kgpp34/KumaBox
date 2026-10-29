//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopySparsePreservesLogicalData(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.raw")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("first"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("last"), 16<<20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "destination.raw")
	if err := CopySparse(destination, source); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("sparse copy changed file data")
	}
}
