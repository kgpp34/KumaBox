package operation

import (
	"context"
	"errors"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestJournalRecordsAndRecoversRunningOperation(t *testing.T) {
	engine, err := meta.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	}()
	journal := NewWithEngine(engine)
	ctx := context.Background()
	started, err := journal.Begin(ctx, "op-1", "run", "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != StatusRunning || started.Attempt != 1 {
		t.Fatalf("started = %+v", started)
	}
	recoverable, err := journal.Recoverable(ctx)
	if err != nil || len(recoverable) != 1 {
		t.Fatalf("recoverable = %+v, err = %v", recoverable, err)
	}
	finished, err := journal.Complete(ctx, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != StatusSuccess || finished.FinishedAt == nil {
		t.Fatalf("finished = %+v", finished)
	}
	recoverable, err = journal.Recoverable(ctx)
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("recoverable after completion = %+v, err = %v", recoverable, err)
	}
}

func TestJournalReconcilePublishesRepairResult(t *testing.T) {
	engine, err := meta.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	}()
	journal := NewWithEngine(engine)
	ctx := context.Background()
	if _, err := journal.Begin(ctx, "op-ok", "delete", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Begin(ctx, "op-fail", "network", "vm-2"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Reconcile(ctx, func(_ context.Context, record Record) error {
		if record.ID == "op-fail" {
			return errors.New("host cleanup pending")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if recoverable, err := journal.Recoverable(ctx); err != nil || len(recoverable) != 0 {
		t.Fatalf("recoverable after reconcile = %+v, err = %v", recoverable, err)
	}
}

func TestJournalPreservesRelatedResourceDuringRecovery(t *testing.T) {
	engine, err := meta.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	}()
	journal := NewWithEngine(engine)
	ctx := context.Background()
	started, err := journal.BeginWithRelated(ctx, "op-restore", KindSnapshotRestoreVM, "vm-1", "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if started.RelatedID != "snap-1" {
		t.Fatalf("related resource = %q", started.RelatedID)
	}
	if err := journal.Reconcile(ctx, func(_ context.Context, record Record) error {
		if record.ResourceID != "vm-1" || record.RelatedID != "snap-1" {
			t.Fatalf("reconcile record = %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
