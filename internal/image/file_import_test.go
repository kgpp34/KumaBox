// SPDX-License-Identifier: MIT

package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLocalCopiesAndInspectsImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "ubuntu-jammy.img")
	content := []byte("cloud image")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	qemuImg := fakeInspectQEMUImg(t, dir, "qcow2", 4096, int64(len(content)))
	destination := filepath.Join(dir, "staging", "base.img")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}

	artifact, err := importLocal(fileImportRequest{Source: source, Destination: destination, QemuImgPath: qemuImg})
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(content)
	if artifact.Path != destination || artifact.Format != "qcow2" || artifact.VirtualSizeBytes != 4096 {
		t.Fatalf("artifact = %+v", artifact)
	}
	if artifact.SHA256 != hex.EncodeToString(expected[:]) || artifact.ActualSizeBytes != int64(len(content)) {
		t.Fatalf("artifact digest and size = %+v", artifact)
	}
	if osFamily(source) != "ubuntu" || diskExtension(artifact.Format) != "qcow2" {
		t.Fatalf("source helpers returned unexpected values")
	}
}

func TestRemoteCopiesFileURLAndChecksDigest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "noble.img")
	content := []byte("file URL image")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(content)
	qemuImg := fakeInspectQEMUImg(t, dir, "raw", 8192, int64(len(content)))
	destination := filepath.Join(dir, "base.img")

	artifact, err := importRemote(fileImportRequest{
		Source:         "file://" + source,
		Destination:    destination,
		QemuImgPath:    qemuImg,
		ExpectedSHA256: hex.EncodeToString(expected[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.SourceHint != source || artifact.Format != "raw" {
		t.Fatalf("artifact = %+v", artifact)
	}
}

func TestRemoteRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "image.img")
	if err := os.WriteFile(source, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := importRemote(fileImportRequest{
		Source:         "file://" + source,
		Destination:    filepath.Join(dir, "base.img"),
		QemuImgPath:    fakeInspectQEMUImg(t, dir, "raw", 1024, 5),
		ExpectedSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func fakeInspectQEMUImg(t *testing.T, dir, format string, virtualSize, actualSize int64) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img")
	script := "#!/bin/sh\n" +
		"printf '{\"format\":\"" + format + "\",\"virtual-size\":" + strconv.FormatInt(virtualSize, 10) + ",\"actual-size\":" + strconv.FormatInt(actualSize, 10) + "}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
