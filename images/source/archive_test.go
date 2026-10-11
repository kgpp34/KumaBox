package source

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/images"
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
			if _, _, err := NewArchiveContext(t.Context(), path, staging, images.DefaultLimits()); err == nil {
				t.Fatal("accepted archive link")
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed extraction left staging: %v, %v", entries, err)
			}
		})
	}
}

func bootTar(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
