package runtime

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vm"
)

const (
	VMEventAdded    = "ADDED"
	VMEventModified = "MODIFIED"
	VMEventDeleted  = "DELETED"
)

// VMStatusEvent describes one change in the selected VM set.
type VMStatusEvent struct {
	Event string       `json:"event"`
	VM    *vm.VMRecord `json:"vm"`
}

// VMStatusUpdate is emitted only when the selected VM status changes.
type VMStatusUpdate struct {
	Records []*vm.VMRecord
	Events  []VMStatusEvent
}

// WatchVMs emits an initial snapshot and then status changes until ctx is
// cancelled. Metadata events reduce latency; polling remains the correctness
// mechanism because both JSON and SQLite engines may coalesce notifications.
func (r *Runtime) WatchVMs(
	ctx context.Context,
	refs []string,
	interval time.Duration,
	emit func(VMStatusUpdate) error,
) error {
	if interval <= 0 {
		return fmt.Errorf("watch interval must be positive")
	}
	if emit == nil {
		return fmt.Errorf("watch emitter is required")
	}

	changes, release := r.subscribeVMEvents(ctx)
	defer release()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var previous map[string]vmStatusEntry
	for {
		if ctx.Err() != nil {
			return nil
		}
		records, err := r.listSelectedVMs(refs)
		if err != nil {
			return err
		}
		current := snapshotVMStatuses(records)
		events := diffVMStatuses(previous, current)
		if previous == nil || len(events) > 0 {
			if err := emit(VMStatusUpdate{Records: records, Events: events}); err != nil {
				return err
			}
		}
		previous = current

		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changes:
			if !ok {
				changes = nil
			}
		case <-ticker.C:
		}
	}
}

func (r *Runtime) listSelectedVMs(refs []string) ([]*vm.VMRecord, error) {
	records, err := r.ListVMs()
	if err != nil || len(refs) == 0 {
		return records, err
	}
	selected := make([]*vm.VMRecord, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		for _, record := range records {
			if record.ID != ref && record.Name != ref {
				continue
			}
			if _, ok := seen[record.ID]; !ok {
				selected = append(selected, record)
				seen[record.ID] = struct{}{}
			}
			break
		}
	}
	return selected, nil
}

func (r *Runtime) subscribeVMEvents(ctx context.Context) (<-chan struct{}, func()) {
	source, ok := r.vmReader.(state.VMEvents)
	if !ok {
		return nil, func() {}
	}
	changes, release, err := source.Events(ctx)
	if err != nil {
		return nil, func() {}
	}
	return changes, release
}

type vmStatusEntry struct {
	record   *vm.VMRecord
	snapshot vmStatusSnapshot
}

type vmStatusSnapshot struct {
	Name           string
	State          vm.VMState
	ObservedState  vm.ObservedState
	ObservedReason string
	Backend        string
	PID            int
	Error          string
	UpdatedAt      time.Time
}

func snapshotVMStatuses(records []*vm.VMRecord) map[string]vmStatusEntry {
	result := make(map[string]vmStatusEntry, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		result[record.ID] = vmStatusEntry{
			record: record,
			snapshot: vmStatusSnapshot{
				Name: record.Name, State: record.State,
				ObservedState: record.ObservedState, ObservedReason: record.ObservedReason,
				Backend: record.Backend, PID: record.PID, Error: record.Error,
				UpdatedAt: record.UpdatedAt,
			},
		}
	}
	return result
}

func diffVMStatuses(previous, current map[string]vmStatusEntry) []VMStatusEvent {
	ids := make([]string, 0, len(previous)+len(current))
	seen := make(map[string]struct{}, len(previous)+len(current))
	for id := range current {
		ids = append(ids, id)
		seen[id] = struct{}{}
	}
	for id := range previous {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	events := make([]VMStatusEvent, 0, len(ids))
	for _, id := range ids {
		before, existed := previous[id]
		after, exists := current[id]
		switch {
		case !existed && exists:
			events = append(events, VMStatusEvent{Event: VMEventAdded, VM: after.record})
		case existed && !exists:
			events = append(events, VMStatusEvent{Event: VMEventDeleted, VM: before.record})
		case before.snapshot != after.snapshot:
			events = append(events, VMStatusEvent{Event: VMEventModified, VM: after.record})
		}
	}
	return events
}
