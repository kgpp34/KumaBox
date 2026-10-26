package firecracker

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkPrepareKernel measures repeated starts, with boot artifacts already
// cached. Large ELF bodies must not be copied into the Go heap just to inspect
// their headers; compressed kernels still require content-addressed cache keys.
func BenchmarkPrepareKernel(b *testing.B) {
	for _, format := range []string{"ELF", "CachedBZImage"} {
		b.Run(format, func(b *testing.B) {
			raw := make([]byte, 32<<20)
			copy(raw, elfFixture())
			if format == "CachedBZImage" {
				var compressed bytes.Buffer
				compressed.WriteString("boot prefix")
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(raw); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
				raw = compressed.Bytes()
			}
			source := filepath.Join(b.TempDir(), "vmlinuz")
			if err := os.WriteFile(source, raw, 0o600); err != nil {
				b.Fatal(err)
			}
			cache := b.TempDir()
			if _, err := prepareKernel(b.Context(), cache, source); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := prepareKernel(b.Context(), cache, source); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
