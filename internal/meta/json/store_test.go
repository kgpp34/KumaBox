package json

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestStoreCommitsAndRollsBack(t *testing.T) {
	store, dir := newTestStore(t)
	ctx := context.Background()

	if err := store.Update(ctx, meta.Scope{Write: "vm"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vm", "records", "vm-1", stdjson.RawMessage(`{"name":"one"}`))
	}); err != nil {
		t.Fatalf("initial update: %v", err)
	}

	wantErr := errors.New("abort")
	if err := store.Update(ctx, meta.Scope{Write: "vm"}, meta.CommitDurable, func(w meta.Writer) error {
		if err := w.PutRaw(ctx, "vm", "records", "vm-2", stdjson.RawMessage(`{"name":"two"}`)); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback error = %v, want %v", err, wantErr)
	}

	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		if _, ok, err := r.GetRaw(ctx, "vm", "records", "vm-2"); err != nil {
			return err
		} else if ok {
			t.Fatal("rolled-back record is visible")
		}
		return nil
	}); err != nil {
		t.Fatalf("view after rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vm.json")); err != nil {
		t.Fatalf("committed metadata file missing: %v", err)
	}
}

func TestStorePreservesPreviousGenerationAndRecovers(t *testing.T) {
	store, dir := newTestStore(t)
	ctx := context.Background()
	put := func(name string) error {
		return store.Update(ctx, meta.Scope{Write: "vm"}, meta.CommitDurable, func(w meta.Writer) error {
			return w.PutRaw(ctx, "vm", "records", "vm-1", stdjson.RawMessage(`{"name":"`+name+`"}`))
		})
	}
	if err := put("one"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := put("two"); err != nil {
		t.Fatalf("second update: %v", err)
	}

	path := filepath.Join(dir, "vm.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt main generation: %v", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		raw, ok, err := r.GetRaw(ctx, "vm", "records", "vm-1")
		if err != nil {
			return err
		}
		if !ok || !sameJSON(raw, []byte(`{"name":"one"}`)) {
			t.Fatalf("recovered record = %s, present=%v", raw, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("view from previous generation: %v", err)
	}
	if err := put("three"); err != nil {
		t.Fatalf("repair update: %v", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		raw, _, err := r.GetRaw(ctx, "vm", "records", "vm-1")
		if err != nil {
			return err
		}
		if !sameJSON(raw, []byte(`{"name":"three"}`)) {
			t.Fatalf("repaired record = %s", raw)
		}
		return nil
	}); err != nil {
		t.Fatalf("view after repair: %v", err)
	}
}

func TestStoreEnforcesScopeAndDetachedValues(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if err := store.Update(ctx, meta.Scope{Write: "vm", Read: []meta.Namespace{"network"}}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "network", "leases", "ip-1", stdjson.RawMessage(`{}`))
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("write scope error = %v, want ErrScope", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		_, _, err := r.GetRaw(ctx, "network", "leases", "ip-1")
		return err
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("read scope error = %v, want ErrScope", err)
	}

	if err := store.Update(ctx, meta.Scope{Write: "vm"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vm", "records", "vm-1", stdjson.RawMessage(`{"n":1}`))
	}); err != nil {
		t.Fatalf("seed update: %v", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		raw, _, err := r.GetRaw(ctx, "vm", "records", "vm-1")
		if err != nil {
			return err
		}
		raw[0] = 'X'
		return nil
	}); err != nil {
		t.Fatalf("detached read: %v", err)
	}
	if err := store.View(ctx, []meta.Namespace{"vm"}, func(r meta.Reader) error {
		raw, _, err := r.GetRaw(ctx, "vm", "records", "vm-1")
		if err != nil {
			return err
		}
		if !sameJSON(raw, []byte(`{"n":1}`)) {
			t.Fatalf("stored record mutated: %s", raw)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify detached read: %v", err)
	}
}

func TestStoreEventsCoalesce(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	ch, release, err := store.Events(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer release()
	for i := 0; i < 3; i++ {
		if err := store.Update(ctx, meta.Scope{Write: "vm"}, meta.CommitRelaxed, func(w meta.Writer) error {
			return w.PutRaw(ctx, "vm", "records", meta.RecordID(string(rune('a'+i))), stdjson.RawMessage(`{}`))
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
		t.Fatal("event channel should coalesce notifications")
	default:
	}
}

func TestStoreEventsObserveAnotherStoreProcess(t *testing.T) {
	_, dir := newTestStore(t)
	open := func() *Store {
		store, err := Open(Namespace{
			Name: "vm", FilePath: filepath.Join(dir, "vm.json"), LockPath: filepath.Join(dir, "vm.lock"),
			Codec: TableCodec{Specs: []TableSpec{{Key: "records", Table: "records"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	reader, writer := open(), open()
	changes, release, err := reader.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := writer.Update(t.Context(), meta.Scope{Write: "vm"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(t.Context(), "vm", "records", "external", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not observe external JSON store commit")
	}
}

func TestStoreEventsReleaseMayRaceClose(t *testing.T) {
	store, _ := newTestStore(t)
	_, release, err := store.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); release() }()
	go func() { defer wait.Done(); _ = store.Close() }()
	wait.Wait()
}

func TestStoreRejectsCorruptMetadataWithoutPreviousGeneration(t *testing.T) {
	store, dir := newTestStore(t)
	path := filepath.Join(dir, "vm.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.View(context.Background(), []meta.Namespace{"vm"}, func(meta.Reader) error { return nil }); !errors.Is(err, meta.ErrCorrupt) {
		t.Fatalf("corrupt error = %v, want ErrCorrupt", err)
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(
		Namespace{
			Name:     "vm",
			FilePath: filepath.Join(dir, "vm.json"),
			LockPath: filepath.Join(dir, "vm.lock"),
			Codec:    TableCodec{Specs: []TableSpec{{Key: "records", Table: "records"}}},
		},
		Namespace{
			Name:     "network",
			FilePath: filepath.Join(dir, "network.json"),
			LockPath: filepath.Join(dir, "network.lock"),
			Codec:    TableCodec{Specs: []TableSpec{{Key: "leases", Table: "leases"}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

func sameJSON(left, right []byte) bool {
	var normalizedLeft, normalizedRight bytes.Buffer
	if err := stdjson.Compact(&normalizedLeft, left); err != nil {
		return false
	}
	if err := stdjson.Compact(&normalizedRight, right); err != nil {
		return false
	}
	return bytes.Equal(normalizedLeft.Bytes(), normalizedRight.Bytes())
}
