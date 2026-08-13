// Package metering records durable VM compute lifecycle events and derives
// usage intervals from them.
package metering

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/metastore"
	metajson "github.com/kumabox/kumabox/internal/metastore/json"
)

const (
	Namespace metastore.Namespace = "metering"
	Table     metastore.Table     = "events"

	KindComputeStart Kind = "vm.compute.start"
	KindComputeStop  Kind = "vm.compute.stop"

	ReasonBoot      Reason = "boot"
	ReasonRestart   Reason = "restart"
	ReasonResume    Reason = "resume"
	ReasonClone     Reason = "clone"
	ReasonRestore   Reason = "restore"
	ReasonHibernate Reason = "hibernate"
	ReasonPause     Reason = "pause"
	ReasonStopUser  Reason = "stop-user"
	ReasonStopCrash Reason = "stop-crash"
	ReasonDelete    Reason = "vm-delete"
)

type Kind string
type Reason string

type Shape struct {
	VCPUs       int   `json:"vcpus"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// Event is one append-only compute lifecycle endpoint.
type Event struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	VMID      string    `json:"vmId"`
	VMName    string    `json:"vmName"`
	Reason    Reason    `json:"reason"`
	Shape     Shape     `json:"shape"`
	EmittedAt time.Time `json:"emittedAt"`
}

// UsageInterval is one paired compute start/stop interval. EndedAt is nil
// while the interval remains open.
type UsageInterval struct {
	VMID        string     `json:"vmId"`
	VMName      string     `json:"vmName"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	VCPUs       int        `json:"vcpus"`
	MemoryBytes int64      `json:"memoryBytes"`
	StartReason Reason     `json:"startReason"`
	EndReason   Reason     `json:"endReason,omitempty"`
}

type Query struct {
	VMRef string
	Since *time.Time
	Until *time.Time
}

type Store struct {
	engine     metastore.MetaEngine
	collection *metastore.Collection[Event]
}

func New(rootDir string) *Store {
	engine, err := metajson.Open(JSONNamespace(rootDir))
	if err != nil {
		panic(fmt.Sprintf("open metering metadata: %v", err))
	}
	return NewWithEngine(engine)
}

func JSONNamespace(rootDir string) metajson.Namespace {
	return metajson.Namespace{
		Name: string(Namespace), FilePath: filepath.Join(rootDir, "metering", "events.json"),
		LockPath: filepath.Join(rootDir, "metering", "events.lock"),
		Codec:    metajson.TableCodec{Specs: []metajson.TableSpec{{Key: string(Table), Table: string(Table)}}},
	}
}

func NewWithEngine(engine metastore.MetaEngine) *Store {
	return &Store{engine: engine, collection: metastore.NewCollection[Event](Namespace, Table)}
}

func (s *Store) MetadataEngine() metastore.MetaEngine { return s.engine }

// Append inserts one event. Repeating the same ID with the same value is
// idempotent; conflicting reuse fails closed.
func (s *Store) Append(ctx context.Context, event Event) error {
	if event.ID == "" || event.VMID == "" || event.VMName == "" || event.EmittedAt.IsZero() {
		return fmt.Errorf("metering event identity and timestamp are required: %w", metastore.ErrScope)
	}
	if event.Kind != KindComputeStart && event.Kind != KindComputeStop {
		return fmt.Errorf("unknown metering event kind %q: %w", event.Kind, metastore.ErrScope)
	}
	event.EmittedAt = event.EmittedAt.UTC()
	return s.engine.Update(ctx, metastore.Scope{Write: Namespace}, metastore.CommitDurable, func(writer metastore.Writer) error {
		existing, err := s.collection.Get(ctx, writer, metastore.RecordID(event.ID))
		if err == nil {
			if equalEvent(*existing, event) {
				return nil
			}
			return fmt.Errorf("metering event id %q has conflicting content: %w", event.ID, metastore.ErrConflict)
		}
		if !errors.Is(err, metastore.ErrNotFound) {
			return err
		}
		return s.collection.Insert(ctx, writer, metastore.RecordID(event.ID), &event)
	})
}

func (s *Store) Events(ctx context.Context, vmRef string) ([]Event, error) {
	events := make([]Event, 0)
	err := s.engine.View(ctx, []metastore.Namespace{Namespace}, func(reader metastore.Reader) error {
		return s.collection.Scan(ctx, reader, func(_ metastore.RecordID, event *Event) error {
			if vmRef == "" || event.VMID == vmRef || event.VMName == vmRef {
				events = append(events, *event)
			}
			return nil
		})
	})
	sort.Slice(events, func(i, j int) bool {
		if events[i].EmittedAt.Equal(events[j].EmittedAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].EmittedAt.Before(events[j].EmittedAt)
	})
	return events, err
}

func (s *Store) Usage(ctx context.Context, query Query) ([]UsageInterval, error) {
	events, err := s.Events(ctx, query.VMRef)
	if err != nil {
		return nil, err
	}
	open := make(map[string]*UsageInterval)
	intervals := make([]UsageInterval, 0)
	for _, event := range events {
		switch event.Kind {
		case KindComputeStart:
			if current := open[event.VMID]; current != nil {
				endedAt := event.EmittedAt
				current.EndedAt = &endedAt
				current.EndReason = ReasonStopCrash
				intervals = append(intervals, *current)
			}
			open[event.VMID] = &UsageInterval{VMID: event.VMID, VMName: event.VMName, StartedAt: event.EmittedAt, VCPUs: event.Shape.VCPUs, MemoryBytes: event.Shape.MemoryBytes, StartReason: event.Reason}
		case KindComputeStop:
			current := open[event.VMID]
			if current == nil || event.EmittedAt.Before(current.StartedAt) {
				continue
			}
			endedAt := event.EmittedAt
			current.EndedAt = &endedAt
			current.EndReason = event.Reason
			intervals = append(intervals, *current)
			delete(open, event.VMID)
		}
	}
	for _, current := range open {
		intervals = append(intervals, *current)
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].StartedAt.Before(intervals[j].StartedAt) })
	return filterIntervals(intervals, query.Since, query.Until), nil
}

func EventID(vmID string, kind Kind, at time.Time) string {
	return fmt.Sprintf("%s:%s:%d", vmID, kind, at.UTC().UnixNano())
}

func equalEvent(a, b Event) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.VMID == b.VMID && a.VMName == b.VMName && a.Reason == b.Reason && a.Shape == b.Shape && a.EmittedAt.Equal(b.EmittedAt)
}

func filterIntervals(intervals []UsageInterval, since, until *time.Time) []UsageInterval {
	result := make([]UsageInterval, 0, len(intervals))
	for _, interval := range intervals {
		if until != nil && !interval.StartedAt.Before(*until) {
			continue
		}
		if since != nil && interval.EndedAt != nil && !interval.EndedAt.After(*since) {
			continue
		}
		result = append(result, interval)
	}
	return result
}
