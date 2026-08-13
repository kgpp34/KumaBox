package metastore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMemoryEngineCommitsAndRollsBack(t *testing.T) {
	engine := newTestEngine(t)
	ctx := context.Background()

	if err := engine.Update(ctx, Scope{Write: "vm"}, CommitDurable, func(w Writer) error {
		return w.PutRaw(ctx, "vm", "records", "vm-1", json.RawMessage(`{"name":"one"}`))
	}); err != nil {
		t.Fatalf("initial update: %v", err)
	}

	wantErr := errors.New("abort")
	err := engine.Update(ctx, Scope{Write: "vm"}, CommitDurable, func(w Writer) error {
		if err := w.PutRaw(ctx, "vm", "records", "vm-2", json.RawMessage(`{"name":"two"}`)); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("rollback error = %v, want %v", err, wantErr)
	}

	if err := engine.View(ctx, []Namespace{"vm"}, func(r Reader) error {
		_, ok, err := r.GetRaw(ctx, "vm", "records", "vm-2")
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("rolled-back record is visible")
		}
		return nil
	}); err != nil {
		t.Fatalf("view after rollback: %v", err)
	}
}

func TestMemoryEngineEnforcesWriteScope(t *testing.T) {
	engine := newTestEngine(t)
	ctx := context.Background()
	err := engine.Update(ctx, Scope{Write: "vm", Read: []Namespace{"network"}}, CommitDurable, func(w Writer) error {
		return w.PutRaw(ctx, "network", "leases", "10.0.0.2", json.RawMessage(`{}`))
	})
	if !errors.Is(err, ErrScope) {
		t.Fatalf("scope error = %v, want ErrScope", err)
	}
}

func TestMemoryEngineEnforcesReadScope(t *testing.T) {
	engine := newTestEngine(t)
	ctx := context.Background()
	err := engine.View(ctx, []Namespace{"vm"}, func(r Reader) error {
		_, _, err := r.GetRaw(ctx, "network", "leases", "10.0.0.2")
		return err
	})
	if !errors.Is(err, ErrScope) {
		t.Fatalf("scope error = %v, want ErrScope", err)
	}
}

func TestMemoryEngineDetachedValuesAndStableScan(t *testing.T) {
	engine := newTestEngine(t)
	ctx := context.Background()
	if err := engine.Update(ctx, Scope{Write: "vm"}, CommitDurable, func(w Writer) error {
		if err := w.PutRaw(ctx, "vm", "records", "b", json.RawMessage(`{"n":2}`)); err != nil {
			return err
		}
		return w.PutRaw(ctx, "vm", "records", "a", json.RawMessage(`{"n":1}`))
	}); err != nil {
		t.Fatalf("seed update: %v", err)
	}

	if err := engine.View(ctx, []Namespace{"vm"}, func(r Reader) error {
		raw, ok, err := r.GetRaw(ctx, "vm", "records", "a")
		if err != nil || !ok {
			return errors.New("record a missing")
		}
		raw[0] = 'X'
		ids := make([]string, 0, 2)
		if err := r.ScanRaw(ctx, "vm", "records", func(id RecordID, _ json.RawMessage) error {
			ids = append(ids, string(id))
			return nil
		}); err != nil {
			return err
		}
		if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
			return errors.New("scan order is not stable")
		}
		return nil
	}); err != nil {
		t.Fatalf("detached view: %v", err)
	}

	if err := engine.View(ctx, []Namespace{"vm"}, func(r Reader) error {
		raw, _, err := r.GetRaw(ctx, "vm", "records", "a")
		if err != nil {
			return err
		}
		if string(raw) != `{"n":1}` {
			t.Fatalf("stored value mutated through reader: %s", raw)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify detached value: %v", err)
	}
}

func TestCollectionPersistsTypedDetachedRecords(t *testing.T) {
	type record struct {
		Name string `json:"name"`
	}

	engine := newTestEngine(t)
	collection := NewCollection[record]("vm", "records")
	ctx := context.Background()
	if err := engine.Update(ctx, Scope{Write: "vm"}, CommitDurable, func(writer Writer) error {
		return collection.Upsert(ctx, writer, "vm-1", &record{Name: "one"})
	}); err != nil {
		t.Fatalf("typed update: %v", err)
	}

	if err := engine.View(ctx, []Namespace{"vm"}, func(reader Reader) error {
		got, err := collection.Get(ctx, reader, "vm-1")
		if err != nil {
			return err
		}
		got.Name = "mutated outside transaction"
		return nil
	}); err != nil {
		t.Fatalf("typed view: %v", err)
	}

	if err := engine.View(ctx, []Namespace{"vm"}, func(reader Reader) error {
		got, err := collection.Get(ctx, reader, "vm-1")
		if err != nil {
			return err
		}
		if got.Name != "one" {
			t.Fatalf("typed record was not detached: %q", got.Name)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify typed record: %v", err)
	}
}

func TestMemoryEngineEventsAreCoalesced(t *testing.T) {
	engine := newTestEngine(t)
	ctx := context.Background()
	ch, release, err := engine.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer release()

	for i := 0; i < 3; i++ {
		if err := engine.Update(ctx, Scope{Write: "vm"}, CommitRelaxed, func(w Writer) error {
			return w.PutRaw(ctx, "vm", "records", RecordID(string(rune('a'+i))), json.RawMessage(`{}`))
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	select {
	case <-ch:
	default:
		t.Fatal("expected metadata event")
	}
	select {
	case <-ch:
		t.Fatal("event channel should coalesce pending notifications")
	default:
	}
}

func TestMemoryEngineContextCancellation(t *testing.T) {
	engine := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.View(ctx, []Namespace{"vm"}, func(Reader) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("view error = %v, want context.Canceled", err)
	}
}

func newTestEngine(t *testing.T) *MemoryEngine {
	t.Helper()
	engine, err := NewMemoryEngine("vm", "network")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}
