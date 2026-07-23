package operation

import (
	"context"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestJournalRecordsAndRecoversRunningOperation(t *testing.T) {
	engine, err := meta.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
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
