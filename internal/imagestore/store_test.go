// SPDX-License-Identifier: MIT

package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStoreCreateListInspectAndResolve(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "data"))

	rec, err := store.Create(CreateRequest{
		Name:   "ubuntu",
		Source: Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: RootDisk{
			Path:   "base.qcow2",
			Format: "qcow2",
		},
		Boot: Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.ID, "img_") {
		t.Fatalf("image id = %s", rec.ID)
	}

	byName, err := store.Inspect("ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != rec.ID || byName.RootDisk.Format != "qcow2" {
		t.Fatalf("inspect by name = %+v", byName)
	}

	byPrefix, err := store.Inspect(rec.ID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if byPrefix.ID != rec.ID {
		t.Fatalf("inspect by prefix = %+v", byPrefix)
	}

	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "ubuntu" {
		t.Fatalf("records = %+v", records)
	}
}

func TestStoreRejectsDuplicateImageName(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "data"))

	if _, err := store.Create(CreateRequest{Name: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateRequest{Name: "ubuntu"}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("expected ErrNameConflict, got %v", err)
	}
}

func TestResolveAmbiguousImagePrefix(t *testing.T) {
	idx := &imageIndex{
		Images: map[string]*ImageRecord{
			"img_abcdef1111111111": {ID: "img_abcdef1111111111"},
			"img_abcdef2222222222": {ID: "img_abcdef2222222222"},
		},
	}

	if _, err := idx.resolve("img_abcdef"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ambiguous ref, got %v", err)
	}
}

func TestImportLocalCommitsImageAndManifests(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "fixtures", "jammy-server-cloudimg-amd64.img")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceContent := []byte("cloud image")
	if err := os.WriteFile(source, sourceContent, 0o644); err != nil {
		t.Fatal(err)
	}
	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := New(filepath.Join(dir, "data"))
	rec, err := store.ImportLocal(ImportRequest{
		Name:        "ubuntu",
		File:        source,
		Firmware:    firmware,
		QemuImgPath: fakeQemuImg(t, dir, "qcow2", 4096, int64(len(sourceContent))),
	})
	if err != nil {
		t.Fatal(err)
	}

	if rec.Name != "ubuntu" || rec.Source.Type != "local-file" || rec.Source.URI != source {
		t.Fatalf("record source = %+v", rec)
	}
	if rec.RootDisk.Format != "qcow2" || rec.RootDisk.VirtualSizeBytes != 4096 {
		t.Fatalf("root disk = %+v", rec.RootDisk)
	}
	expectedSum := sha256.Sum256(sourceContent)
	if rec.RootDisk.SHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("sha256 = %s", rec.RootDisk.SHA256)
	}
	if _, err := os.Stat(rec.RootDisk.Path); err != nil {
		t.Fatalf("committed root disk missing: %v", err)
	}
	if !strings.Contains(rec.RootDisk.Path, string(filepath.Separator)+"cloudimg"+string(filepath.Separator)) {
		t.Fatalf("root disk path = %s, want cloudimg store", rec.RootDisk.Path)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(rec.RootDisk.Path), "image.json")); err != nil {
		t.Fatalf("image manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(rec.RootDisk.Path), "source.json")); err != nil {
		t.Fatalf("source manifest missing: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source image should remain: %v", err)
	}

	inspected, err := store.Inspect("ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if inspected.ID != rec.ID {
		t.Fatalf("inspect id = %s, want %s", inspected.ID, rec.ID)
	}
}

func TestImportLocalDoesNotIndexFailedInspect(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "ubuntu.img")
	if err := os.WriteFile(source, []byte("cloud image"), 0o644); err != nil {
		t.Fatal(err)
	}
	firmware := filepath.Join(dir, "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := New(filepath.Join(dir, "data"))
	_, err := store.ImportLocal(ImportRequest{
		Name:        "bad",
		File:        source,
		Firmware:    firmware,
		QemuImgPath: fakeFailingQemuImg(t, dir),
	})
	if err == nil {
		t.Fatal("expected import failure")
	}
	records, listErr := store.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(records) != 0 {
		t.Fatalf("failed import should not update index: %+v", records)
	}
}

func TestPullDownloadsHTTPURLAndCommitsImage(t *testing.T) {
	dir := t.TempDir()
	content := []byte("cloud image from http")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jammy-server-cloudimg-amd64.img" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()

	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.MkdirAll(filepath.Dir(firmware), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	expectedSum := sha256.Sum256(content)
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Pull(PullRequest{
		Name:        "ubuntu-http",
		URL:         server.URL + "/jammy-server-cloudimg-amd64.img",
		Firmware:    firmware,
		QemuImgPath: fakeQemuImg(t, dir, "qcow2", 8192, int64(len(content))),
		SHA256:      hex.EncodeToString(expectedSum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}

	if rec.Name != "ubuntu-http" || rec.Source.Type != "url" {
		t.Fatalf("record source = %+v", rec)
	}
	if rec.RootDisk.Format != "qcow2" || rec.RootDisk.VirtualSizeBytes != 8192 {
		t.Fatalf("root disk = %+v", rec.RootDisk)
	}
	if rec.RootDisk.SHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("sha256 = %s", rec.RootDisk.SHA256)
	}
	committed, err := os.ReadFile(rec.RootDisk.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(content) {
		t.Fatalf("committed disk content = %q", committed)
	}
}

func TestPullCopiesFileURLAndCommitsImage(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "fixtures", "noble-server-cloudimg-amd64.img")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("file url image"), 0o644); err != nil {
		t.Fatal(err)
	}
	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := New(filepath.Join(dir, "data"))
	rec, err := store.Pull(PullRequest{
		Name:        "ubuntu-file",
		URL:         "file://" + source,
		Firmware:    firmware,
		QemuImgPath: fakeQemuImg(t, dir, "raw", 4096, 14),
	})
	if err != nil {
		t.Fatal(err)
	}

	if rec.RootDisk.Format != "raw" || filepath.Base(rec.RootDisk.Path) != "base.raw" {
		t.Fatalf("root disk = %+v", rec.RootDisk)
	}
	if rec.OS.Family != "ubuntu" {
		t.Fatalf("os = %+v", rec.OS)
	}
}

func TestPullRejectsChecksumMismatchWithoutIndexUpdate(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("unexpected content"))
	}))
	defer server.Close()

	firmware := filepath.Join(dir, "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := New(filepath.Join(dir, "data"))
	_, err := store.Pull(PullRequest{
		Name:        "bad-checksum",
		URL:         server.URL + "/image.img",
		Firmware:    firmware,
		QemuImgPath: fakeQemuImg(t, dir, "qcow2", 4096, 18),
		SHA256:      strings.Repeat("0", sha256.Size*2),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}

	records, listErr := store.List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(records) != 0 {
		t.Fatalf("failed pull should not update index: %+v", records)
	}
}

func fakeQemuImg(t *testing.T, dir, format string, virtualSize, actualSize int64) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img")
	script := "#!/bin/sh\n" +
		"printf '{\"format\":\"" + format + "\",\"virtual-size\":" + strconv.FormatInt(virtualSize, 10) + ",\"actual-size\":" + strconv.FormatInt(actualSize, 10) + "}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeFailingQemuImg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img-fail")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
