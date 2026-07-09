// SPDX-License-Identifier: MIT

package ocibuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/ocistore"
)

func TestEnsureEROFSBuildsAndReusesLayer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar.gz")
	layerBytes := gzipTar(t, map[string]string{"hello.txt": "hello"})
	if err := os.WriteFile(layerPath, layerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(layerBytes)
	layerDigest := "sha256:" + hex.EncodeToString(sum[:])

	mkfs := filepath.Join(dir, "mkfs.erofs")
	if err := os.WriteFile(mkfs, []byte("#!/bin/sh\ncp \"$3\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	builder := New(dir)
	rec, err := builder.ensureEROFS(context.Background(), mkfs, ocistore.BlobRecord{
		Digest:    layerDigest,
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		SizeBytes: int64(len(layerBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Filesystem != "erofs" || rec.SourceLayer != layerDigest {
		t.Fatalf("unexpected EROFS record: %+v", rec)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rec.Path) != strings.TrimPrefix(layerDigest, "sha256:")+".erofs" {
		t.Fatalf("unexpected EROFS path: %s", rec.Path)
	}

	cached, err := builder.ensureEROFS(context.Background(), mkfs, ocistore.BlobRecord{
		Digest:    layerDigest,
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		SizeBytes: int64(len(layerBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cached.Path != rec.Path || cached.Digest != rec.Digest {
		t.Fatalf("cached record = %+v, want %+v", cached, rec)
	}
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		body := []byte(content)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
