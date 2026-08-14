package metering

import (
	"errors"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestAppendIsIdempotentAndRejectsConflict(t *testing.T) {
	store := New(t.TempDir())
	at := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	event := Event{ID: EventID("vm-1", KindComputeStart, at), Kind: KindComputeStart, VMID: "vm-1", VMName: "demo", Reason: ReasonBoot, Shape: Shape{VCPUs: 2, MemoryBytes: 1024}, EmittedAt: at}
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.Reason = ReasonRestart
	if err := store.Append(t.Context(), event); !errors.Is(err, meta.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestUsagePairsEventsAndPreservesOpenInterval(t *testing.T) {
	store := New(t.TempDir())
	start := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	stop := start.Add(time.Minute)
	events := []Event{
		{ID: EventID("vm-1", KindComputeStop, stop), Kind: KindComputeStop, VMID: "vm-1", VMName: "demo", Reason: ReasonPause, Shape: Shape{VCPUs: 2, MemoryBytes: 1024}, EmittedAt: stop},
		{ID: EventID("vm-1", KindComputeStart, start), Kind: KindComputeStart, VMID: "vm-1", VMName: "demo", Reason: ReasonBoot, Shape: Shape{VCPUs: 2, MemoryBytes: 1024}, EmittedAt: start},
		{ID: EventID("vm-1", KindComputeStart, stop.Add(time.Minute)), Kind: KindComputeStart, VMID: "vm-1", VMName: "demo", Reason: ReasonResume, Shape: Shape{VCPUs: 2, MemoryBytes: 1024}, EmittedAt: stop.Add(time.Minute)},
	}
	for _, event := range events {
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := store.Usage(t.Context(), Query{VMRef: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage[0].EndedAt == nil || usage[0].EndReason != ReasonPause || usage[1].EndedAt != nil {
		t.Fatalf("usage = %+v", usage)
	}
}
