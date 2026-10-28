package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/metadata/sqlite"
)

// BenchmarkSnapshotAccess measures real WAL/FULL commits on the local filesystem.
// The coalesced case flushes once per 64 uses of one golden snapshot; setup and
// the final partial batch are outside timing. This is a catalog microbenchmark,
// not VM startup latency or a claim about end-to-end throughput.
func BenchmarkSnapshotAccess(b *testing.B) {
	for _, buffered := range []bool{false, true} {
		name := "synchronous"
		if buffered {
			name = "coalesced"
		}
		b.Run(name, func(b *testing.B) {
			engine, err := sqlite.Open(b.Context(), filepath.Join(b.TempDir(), "meta.db"), Collections(), sqlite.DefaultOptions())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = engine.Close() }()
			observed := &accessEngine{Store: engine}
			store := New(observed)
			if buffered {
				store, err = NewBuffered(observed, 64)
				if err != nil {
					b.Fatal(err)
				}
			}
			record := accessSnapshot(b, store, 1)
			observed.updates.Store(0)
			operations := 0
			b.ReportAllocs()
			for b.Loop() {
				operations++
				if _, err := store.Touch(b.Context(), record.ID, record.CreatedAt.Add(time.Duration(operations)*time.Nanosecond)); err != nil {
					b.Fatal(err)
				}
				if buffered && operations%64 == 0 {
					if err := store.FlushAccesses(b.Context()); err != nil {
						b.Fatal(err)
					}
				}
			}
			if err := store.FlushAccesses(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(observed.updates.Load())/float64(operations), "commits/op")
		})
	}
}
