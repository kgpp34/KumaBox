package firecracker

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func elfFixture() []byte {
	raw := make([]byte, 120)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(raw[16:], 2)
	binary.LittleEndian.PutUint16(raw[18:], 62)
	binary.LittleEndian.PutUint32(raw[20:], 1)
	binary.LittleEndian.PutUint64(raw[32:], 64)
	binary.LittleEndian.PutUint16(raw[52:], 64)
	binary.LittleEndian.PutUint16(raw[54:], 56)
	binary.LittleEndian.PutUint16(raw[56:], 1)
	binary.LittleEndian.PutUint32(raw[64:], 1)
	return raw
}

func compressedKernel(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	buffer.Write([]byte("compressed-boot-prefix"))
	stream := gzip.NewWriter(&buffer)
	if _, err := stream.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	buffer.Write([]byte("trailing-boot-data"))
	return buffer.Bytes()
}

func TestKernelCacheConcurrentPublication(t *testing.T) {
	source := filepath.Join(t.TempDir(), "vmlinuz")
	expected := elfFixture()
	if err := os.WriteFile(source, compressedKernel(t, expected), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	var wg sync.WaitGroup
	paths := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := prepareKernel(t.Context(), cache, source)
			if err != nil {
				t.Error(err)
				return
			}
			paths <- p
		}()
	}
	wg.Wait()
	close(paths)
	var first string
	for p := range paths {
		if first == "" {
			first = p
		}
		if p != first {
			t.Fatal("concurrent callers got different kernels")
		}
		raw, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, expected) {
			t.Fatalf("cached ELF: %v", err)
		}
	}
	if first == "" {
		t.Fatal("no kernel published")
	}
	raw, err := os.ReadFile(source)
	if err != nil || validELF(raw) {
		t.Fatal("source kernel was changed")
	}
	if err := os.WriteFile(first, []byte("corrupt cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareKernel(t.Context(), cache, source); err == nil {
		t.Fatal("accepted corrupt cached kernel")
	}
}

func TestKernelExtractionRequiresELF(t *testing.T) {
	for _, raw := range [][]byte{[]byte("no compressed kernel"), compressedKernel(t, []byte{0x7f, 'E', 'L', 'F'}), {0x1f, 0x8b, 0x08}} {
		if _, err := extractKernel(raw); err == nil {
			t.Fatal("accepted invalid kernel")
		}
	}
	source := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(source, elfFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := prepareKernel(t.Context(), t.TempDir(), source)
	if err != nil || result != source {
		t.Fatalf("ELF passthrough: %q %v", result, err)
	}
}

func TestExtractZstdKernel(t *testing.T) {
	var buffer bytes.Buffer
	buffer.WriteString("bzImage prefix")
	encoder, err := zstd.NewWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(elfFixture()); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	buffer.WriteString("boot trailer")
	got, err := extractKernel(buffer.Bytes())
	if err != nil || !bytes.Equal(got, elfFixture()) {
		t.Fatalf("zstd extraction: %v", err)
	}
}
