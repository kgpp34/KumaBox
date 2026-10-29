//go:build linux && integration

package tap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkDatapathHandles isolates the old operation-scoped open/close from
// provider-lifetime borrowing. Both cases use real pins and the same file lock;
// this measures handle management, not packet forwarding or VM readiness.
func BenchmarkDatapathHandles(b *testing.B) {
	p, err := New(Options{
		StateDir: b.TempDir(), PinDir: filepath.Join("/sys/fs/bpf", "kb-bench-"+filepath.Base(b.TempDir())),
		Subnet: "10.255.254.0/24", TAPPrefix: "kb",
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := p.Close(); err != nil {
			b.Error(err)
		}
		if err := os.RemoveAll(p.options.PinDir); err != nil {
			b.Error(err)
		}
	})
	ctx := context.Background()
	_, release, err := p.dataPlane(ctx)
	if err != nil {
		b.Fatal(err)
	}
	release()
	for _, shared := range []bool{false, true} {
		name := "per-operation"
		if shared {
			name = "provider"
		}
		for _, parallel := range []bool{false, true} {
			mode := "/serial"
			if parallel {
				mode = "/parallel"
			}
			b.Run(name+mode, func(b *testing.B) {
				b.ReportAllocs()
				run := func() error {
					if shared {
						_, release, err := p.dataPlane(ctx)
						if err == nil {
							release()
						}
						return err
					}
					d, err := p.loadDatapath(ctx)
					if err != nil {
						return err
					}
					return d.close()
				}
				if parallel {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if err := run(); err != nil {
								b.Error(err)
								return
							}
						}
					})
					return
				}
				for b.Loop() {
					if err := run(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
