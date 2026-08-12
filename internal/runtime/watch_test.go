package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestDiffVMStatusesIgnoresObservationTimestamp(t *testing.T) {
	t.Parallel()

	first := time.Unix(10, 0).UTC()
	second := first.Add(time.Second)
	before := &vmstore.VMRecord{
		ID: "vm-1", Name: "example", State: vmstore.StateRunning,
		ObservedState: vmstore.ObservedStateRunning, ObservedAt: &first,
	}
	after := *before
	after.ObservedAt = &second

	events := diffVMStatuses(snapshotVMStatuses([]*vmstore.VMRecord{before}), snapshotVMStatuses([]*vmstore.VMRecord{&after}))
	if len(events) != 0 {
		t.Fatalf("timestamp-only change emitted events: %+v", events)
	}

	after.State = vmstore.StatePaused
	events = diffVMStatuses(snapshotVMStatuses([]*vmstore.VMRecord{before}), snapshotVMStatuses([]*vmstore.VMRecord{&after}))
	if len(events) != 1 || events[0].Event != VMEventModified {
		t.Fatalf("state change events = %+v", events)
	}
}

func TestWatchVMsUsesMetadataEventsBeforePollingFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(store, backendFake{})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	updates := make(chan VMStatusUpdate, 2)
	errors := make(chan error, 1)
	go func() {
		errors <- rt.WatchVMs(ctx, nil, time.Hour, func(update VMStatusUpdate) error {
			updates <- update
			return nil
		})
	}()

	initial := <-updates
	if len(initial.Records) != 0 || len(initial.Events) != 0 {
		t.Fatalf("initial update = %+v", initial)
	}
	created, err := store.Create(vmstore.CreateRequest{
		Name: "watched", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case update := <-updates:
		if len(update.Events) != 1 || update.Events[0].Event != VMEventAdded || update.Events[0].VM.ID != created.ID {
			t.Fatalf("metadata-triggered update = %+v", update)
		}
		cancel()
	case <-ctx.Done():
		t.Fatal("watch did not wake from metadata event")
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
}

func TestListSelectedVMsPreservesRequestedOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(store, backendFake{})
	for _, name := range []string{"first", "second"} {
		if _, err := store.Create(vmstore.CreateRequest{
			Name: name, RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
			RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := rt.listSelectedVMs([]string{"second", "missing", "first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Name != "second" || records[1].Name != "first" {
		t.Fatalf("selected records = %+v", records)
	}
}

func TestWatchVMsStopsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rt := NewWithBackend(vmstore.New(filepath.Join(dir, "data")), backendFake{})
	ctx, cancel := context.WithCancel(t.Context())
	emitted := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- rt.WatchVMs(ctx, nil, time.Hour, func(VMStatusUpdate) error {
			emitted <- struct{}{}
			return nil
		})
	}()

	<-emitted
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop after context cancellation")
	}
}

func TestWatchVMsRecoversFinalStateAfterCoalescedEvents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(store, backendFake{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	initial := make(chan struct{})
	unblock := make(chan struct{})
	updates := make(chan VMStatusUpdate, 1)
	done := make(chan error, 1)
	go func() {
		first := true
		done <- rt.WatchVMs(ctx, nil, time.Hour, func(update VMStatusUpdate) error {
			if first {
				first = false
				close(initial)
				<-unblock
				return nil
			}
			updates <- update
			return nil
		})
	}()
	<-initial

	first, err := store.Create(vmstore.CreateRequest{
		Name: "first", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(vmstore.CreateRequest{
		Name: "second", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	close(unblock)

	select {
	case update := <-updates:
		if len(update.Records) != 1 || update.Records[0].Name != "second" {
			t.Fatalf("coalesced update did not recover final state: %+v", update)
		}
		cancel()
	case <-time.After(time.Second):
		t.Fatal("watch did not process coalesced metadata event")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
