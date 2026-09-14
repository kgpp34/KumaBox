package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

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

func TestScanBootRespectsWhiteoutsAndRegularFiles(t *testing.T) {
	raw := bootTar(t, []*tar.Header{
		{Name: "boot/vmlinuz-1", Typeflag: tar.TypeReg, Size: 1},
		{Name: "boot/vmlinuz-2", Typeflag: tar.TypeReg, Size: 1},
		{Name: "boot/vmlinuz-2", Typeflag: tar.TypeSymlink, Linkname: "vmlinuz-1"},
		{Name: "boot/vmlinuz.old", Typeflag: tar.TypeReg, Size: 1},
		{Name: "boot/.wh.initrd.img", Typeflag: tar.TypeReg},
		{Name: "boot/.wh..wh..opq", Typeflag: tar.TypeReg},
	})
	files, whiteouts, opaque, err := scanBoot(bytes.NewReader(raw), t.TempDir(), "amd64", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "vmlinuz-1" || len(whiteouts) != 2 || !opaque {
		t.Fatalf("boot scan = %v, %v, %v", files, whiteouts, opaque)
	}
	raw = bootTar(t, []*tar.Header{{Name: "../boot/vmlinuz", Typeflag: tar.TypeReg, Size: 1}})
	if _, _, _, err := scanBoot(bytes.NewReader(raw), t.TempDir(), "amd64", 1024); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestWriteBootFileBoundsARM64Decompression(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("k"), 32)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kernel")
	if err := writeBootFile(bytes.NewReader(compressed.Bytes()), path, true, 32); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) != 32 {
		t.Fatalf("decompressed size = %d, %v", len(raw), err)
	}
	if err := writeBootFile(bytes.NewReader(compressed.Bytes()), path, true, 31); err == nil {
		t.Fatal("accepted oversized kernel")
	}
	if err := writeBootFile(bytes.NewReader(nil), path, false, 32); err == nil {
		t.Fatal("accepted empty kernel")
	}
}
