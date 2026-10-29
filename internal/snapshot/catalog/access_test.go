package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/metadata"
	"github.com/kumabox/kumabox/internal/types"
)

type accessEngine struct {
	metadata.Store
	updates atomic.Int64
	fail    atomic.Bool
	// Hooks are installed before concurrent use and run outside engine locks.
	beforeRead  func()
	afterCommit func()
}

func (s *accessEngine) View(ctx context.Context, read func(metadata.Reader) error) error {
	return s.Store.View(ctx, func(reader metadata.Reader) error {
		if s.beforeRead != nil {
			s.beforeRead()
		}
		return read(reader)
	})
}

func (s *accessEngine) Update(ctx context.Context, write func(metadata.Writer) error) error {
	s.updates.Add(1)
	if s.fail.Load() {
		return errors.New("injected access persistence failure")
	}
	err := s.Store.Update(ctx, write)
	if err == nil && s.afterCommit != nil {
		s.afterCommit()
	}
	return err
}

func accessFixture(t testing.TB, capacity int) (*Store, *accessEngine) {
	t.Helper()
	memory, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memory.Close() })
	engine := &accessEngine{Store: memory}
	store, err := NewBuffered(engine, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return store, engine
}

func accessSnapshot(t testing.TB, store *Store, number int) types.Snapshot {
	t.Helper()
	id := types.SnapshotID(fmt.Sprintf("%08x-e89b-42d3-a456-426614174000", number))
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	record := types.Snapshot{
		ID: id, Name: fmt.Sprintf("snapshot-%d", number), SandboxID: "123e4567-e89b-42d3-a456-426614174000",
		SourceGeneration: 4, ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: created,
	}
	if err := store.Reserve(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	ready, err := store.Commit(context.Background(), id, 42, created)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func TestBufferedAccessCoalescesAndCommitsOneBatch(t *testing.T) {
	store, engine := accessFixture(t, 8)
	first, second := accessSnapshot(t, store, 1), accessSnapshot(t, store, 2)
	engine.updates.Store(0)
	latest := first.CreatedAt.Add(time.Hour)
	for _, event := range []struct {
		id types.SnapshotID
		at time.Time
	}{{first.ID, latest}, {first.ID, latest.Add(-time.Minute)}, {second.ID, latest}} {
		if _, err := store.Touch(t.Context(), event.id, event.at); err != nil {
			t.Fatal(err)
		}
	}
	if engine.updates.Load() != 0 {
		t.Fatal("Touch performed a synchronous write")
	}
	for _, record := range []types.Snapshot{first, second} {
		visible, err := store.Resolve(t.Context(), record.Name)
		if err != nil || !visible.LastAccessedAt.Equal(latest) {
			t.Fatalf("visible access=%s error=%v", visible.LastAccessedAt, err)
		}
		durable, err := New(engine).Resolve(t.Context(), record.Name)
		if err != nil || !durable.LastAccessedAt.Equal(record.CreatedAt) {
			t.Fatalf("premature durable access=%s error=%v", durable.LastAccessedAt, err)
		}
	}
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 2 || !listed[0].LastAccessedAt.Equal(latest) || !listed[1].LastAccessedAt.Equal(latest) {
		t.Fatalf("list=%v error=%v", listed, err)
	}
	if err := store.FlushAccesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAccesses(t.Context()); err != nil || engine.updates.Load() != 1 {
		t.Fatalf("flush count=%d error=%v", engine.updates.Load(), err)
	}
	for _, record := range []types.Snapshot{first, second} {
		durable, err := New(engine).Resolve(t.Context(), record.Name)
		if err != nil || !durable.LastAccessedAt.Equal(latest) {
			t.Fatalf("durable access=%s error=%v", durable.LastAccessedAt, err)
		}
	}
}

func TestBufferedAccessRetriesFailedFlushAndBackpressuresAtCapacity(t *testing.T) {
	store, engine := accessFixture(t, 1)
	first, second := accessSnapshot(t, store, 1), accessSnapshot(t, store, 2)
	accessed := first.CreatedAt.Add(time.Hour)
	if _, err := store.Touch(t.Context(), first.ID, accessed); err != nil {
		t.Fatal(err)
	}
	engine.fail.Store(true)
	if err := store.FlushAccesses(t.Context()); err == nil {
		t.Fatal("failed flush was hidden")
	}
	if _, err := store.Touch(t.Context(), second.ID, accessed); err == nil {
		t.Fatal("saturated buffer silently dropped a usage event")
	}
	visible, err := store.Resolve(t.Context(), first.Name)
	if err != nil || !visible.LastAccessedAt.Equal(accessed) {
		t.Fatalf("failed flush lost pending access: %v %v", visible, err)
	}
	engine.fail.Store(false)
	if _, err := store.Touch(t.Context(), second.ID, accessed); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAccesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, record := range []types.Snapshot{first, second} {
		durable, err := New(engine).Resolve(t.Context(), record.Name)
		if err != nil || !durable.LastAccessedAt.Equal(accessed) {
			t.Fatalf("retried access=%v error=%v", durable, err)
		}
	}
}

func TestBufferedAccessSkipsDeletedAndReusedNames(t *testing.T) {
	store, engine := accessFixture(t, 8)
	first, deleting := accessSnapshot(t, store, 1), accessSnapshot(t, store, 2)
	for _, record := range []types.Snapshot{first, deleting} {
		if _, err := store.Touch(t.Context(), record.ID, record.CreatedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BeginDelete(t.Context(), record.Name); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinalizeDelete(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	replacement := first
	replacement.ID = "00000003-e89b-42d3-a456-426614174000"
	if err := store.Reserve(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(t.Context(), replacement.ID, 42, replacement.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAccesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Resolve(t.Context(), first.Name)
	if err != nil || resolved.ID != replacement.ID || !resolved.LastAccessedAt.Equal(replacement.CreatedAt) {
		t.Fatalf("old access affected replacement: %v %v", resolved, err)
	}
	if _, err := New(engine).Resolve(t.Context(), first.ID.String()); err == nil {
		t.Fatal("deleted snapshot was resurrected")
	}
	state, found, err := store.State(t.Context(), deleting.ID)
	if err != nil || !found || !state.Deleting {
		t.Fatalf("deletion tombstone changed: %v %v %v", state, found, err)
	}
}

func TestBufferedAccessKeepsNewTouchDuringFlush(t *testing.T) {
	store, engine := accessFixture(t, 8)
	record := accessSnapshot(t, store, 1)
	first, newer := record.CreatedAt.Add(time.Hour), record.CreatedAt.Add(2*time.Hour)
	if _, err := store.Touch(t.Context(), record.ID, first); err != nil {
		t.Fatal(err)
	}
	committed, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	engine.afterCommit = func() { close(committed); <-release }
	done := make(chan error, 1)
	go func() { done <- store.FlushAccesses(t.Context()) }()
	select {
	case <-committed:
	case <-time.After(3 * time.Second):
		t.Fatal("flush did not commit")
	}
	if _, err := store.Touch(t.Context(), record.ID, newer); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.FlushAccesses(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting flush did not respect cancellation: %v", err)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	engine.afterCommit = nil // Flush goroutine has joined.
	if err := store.FlushAccesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(engine).Resolve(t.Context(), record.Name)
	if err != nil || !loaded.LastAccessedAt.Equal(newer) {
		t.Fatalf("newer touch lost: %v %v", loaded, err)
	}
}

func TestBufferedAccessRetriesOldReadAcrossFlush(t *testing.T) {
	for _, list := range []bool{false, true} {
		t.Run(fmt.Sprintf("list=%t", list), func(t *testing.T) {
			store, engine := accessFixture(t, 8)
			record := accessSnapshot(t, store, 1)
			latest := record.CreatedAt.Add(time.Hour)
			if _, err := store.Touch(t.Context(), record.ID, latest); err != nil {
				t.Fatal(err)
			}
			reading, release := make(chan struct{}), make(chan struct{})
			var block, unblock sync.Once
			t.Cleanup(func() { unblock.Do(func() { close(release) }) })
			engine.beforeRead = func() { block.Do(func() { close(reading); <-release }) }
			type outcome struct {
				rows []types.Snapshot
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				if list {
					rows, err := store.List(t.Context())
					done <- outcome{rows: rows, err: err}
				} else {
					row, err := store.Resolve(t.Context(), record.Name)
					done <- outcome{rows: []types.Snapshot{row}, err: err}
				}
			}()
			select {
			case <-reading:
			case <-time.After(3 * time.Second):
				t.Fatal("read did not start")
			}
			if err := store.FlushAccesses(t.Context()); err != nil {
				t.Fatal(err)
			}
			unblock.Do(func() { close(release) })
			result := <-done
			if result.err != nil || len(result.rows) != 1 || !result.rows[0].LastAccessedAt.Equal(latest) {
				t.Fatalf("old read missed committed usage: %v %v", result.rows, result.err)
			}
		})
	}
}

func TestBufferedAccessConcurrentTouchesAndFlush(t *testing.T) {
	store, engine := accessFixture(t, 8)
	record := accessSnapshot(t, store, 1)
	var group sync.WaitGroup
	failures := make(chan error, 17)
	for worker := range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			for sequence := range 32 {
				at := record.CreatedAt.Add(time.Duration(worker*32+sequence+1) * time.Second)
				if _, err := store.Touch(t.Context(), record.ID, at); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for range 32 {
			if err := store.FlushAccesses(t.Context()); err != nil {
				failures <- err
				return
			}
		}
	}()
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if err := store.FlushAccesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(engine).Resolve(t.Context(), record.Name)
	if err != nil || !loaded.LastAccessedAt.Equal(record.CreatedAt.Add(512*time.Second)) {
		t.Fatalf("concurrent maximum lost: %v %v", loaded, err)
	}
}
