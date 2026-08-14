// SPDX-License-Identifier: MIT

package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestSplitDigest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("a", 64)
	algo, value, err := splitDigest(valid)
	if err != nil {
		t.Fatalf("splitDigest returned error: %v", err)
	}
	if algo != "sha256" || value != strings.Repeat("a", 64) {
		t.Fatalf("splitDigest = %q %q", algo, value)
	}

	for _, digest := range []string{
		"",
		"sha256:",
		"sha512:" + strings.Repeat("a", 128),
		"sha256:not-hex",
		"sha256:" + strings.Repeat("a", 63),
	} {
		if _, _, err := splitDigest(digest); err == nil {
			t.Fatalf("splitDigest(%q) expected error", digest)
		}
	}
}

func TestEnsureBlobReportsCacheAndAdoptsContent(t *testing.T) {
	t.Parallel()

	engine, err := meta.NewMemoryEngine("oci-content")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithEngine(t.TempDir(), engine)
	content := []byte("content-addressed layer")
	digest := sha256Digest(content)

	first, cached, err := store.ensureBlob(digest, "application/octet-stream", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if cached {
		t.Fatal("new blob reported as cached")
	}
	second, cached, err := store.ensureBlob(digest, "application/octet-stream", bytes.NewReader([]byte("unused")))
	if err != nil {
		t.Fatal(err)
	}
	if !cached || first.Path != second.Path || first.SizeBytes != second.SizeBytes {
		t.Fatalf("cached blob = %+v cached=%t, first=%+v", second, cached, first)
	}
}

func TestEnsureBlobSerializesConcurrentDigestWriters(t *testing.T) {
	t.Parallel()

	engine, err := meta.NewMemoryEngine("oci-content")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithEngine(t.TempDir(), engine)
	content := bytes.Repeat([]byte("layer"), 4096)
	digest := sha256Digest(content)

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := store.ensureBlob(digest, "application/octet-stream", bytes.NewReader(content))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	_, value, err := splitDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.blobsDir, "sha256", value)
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("stored blob length = %d, want %d", len(stored), len(content))
	}
	if got, err := fileSHA256(path); err != nil || "sha256:"+got != digest {
		t.Fatalf("stored digest = sha256:%s, error %v", got, err)
	}
}

func TestPullResultRecordBlob(t *testing.T) {
	t.Parallel()

	var result PullResult
	result.recordBlob(false)
	result.recordBlob(true)
	result.recordBlob(false)
	if result.Downloaded != 2 || result.Cached != 1 {
		t.Fatalf("pull counters = downloaded %d cached %d", result.Downloaded, result.Cached)
	}
}

func sha256Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
