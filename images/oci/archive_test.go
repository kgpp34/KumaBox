package oci

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(path, t.TempDir()); err == nil {
		t.Fatal("extractArchive accepted traversal")
	}
}

func TestRequireEROFSVersion(t *testing.T) {
	if err := requireEROFSVersion("mkfs.erofs 1.8.10"); err != nil {
		t.Fatalf("accepted version: %v", err)
	}
	if err := requireEROFSVersion("mkfs.erofs 1.7"); err == nil {
		t.Fatal("accepted unsafe version")
	}
}

func TestArchiveRejectsLinksAndCleansFailedExtraction(t *testing.T) {
	for _, flag := range []byte{tar.TypeSymlink, tar.TypeLink} {
		t.Run(fmt.Sprint(flag), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "bad.tar")
			raw := bootTar(t, []*tar.Header{{Name: "escape", Typeflag: flag, Linkname: "../outside"}})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			staging := filepath.Join(root, "staging")
			if _, _, err := NewArchiveContext(t.Context(), path, staging, DefaultLimits()); err == nil {
				t.Fatal("accepted archive link")
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed extraction left staging: %v, %v", entries, err)
			}
		})
	}
}
