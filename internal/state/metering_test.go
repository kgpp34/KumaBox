package state

import (
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/metering"
)

func TestMeteringStoreBackendContract(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.Default()
			cfg.Runtime.RootDir = t.TempDir()
			cfg.Metadata.Backend = backend
			if backend == "sqlite" {
				if err := InitSQLiteMetadata(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			stores, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if stores.Metadata != nil {
				defer func() {
					if err := stores.Metadata.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			at := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
			event := metering.Event{ID: metering.EventID("vm-1", metering.KindComputeStart, at), Kind: metering.KindComputeStart, VMID: "vm-1", VMName: "demo", Reason: metering.ReasonBoot, Shape: metering.Shape{VCPUs: 1, MemoryBytes: 512}, EmittedAt: at}
			if err := stores.Metering.Append(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			events, err := stores.Metering.Events(t.Context(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].ID != event.ID {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}
