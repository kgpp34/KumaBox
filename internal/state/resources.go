package state

import (
	"context"

	kbimage "github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/snapshot"
)

// ImageState is the durable image metadata capability used by image
// lifecycle code. Disk import and deletion remain outside this interface's
// persistence responsibility and are coordinated by the caller.
type ImageState interface {
	Create(kbimage.CreateRequest) (*kbimage.ImageRecord, error)
	ImportLocal(kbimage.ImportRequest) (*kbimage.ImageRecord, error)
	Pull(kbimage.PullRequest) (*kbimage.ImageRecord, error)
	Inspect(string) (*kbimage.ImageRecord, error)
	List() ([]*kbimage.ImageRecord, error)
	Remove(kbimage.RemoveRequest) (*kbimage.ImageRecord, error)
}

// SnapshotState is the durable snapshot index and payload lifecycle
// capability. Build and lease types make publication and reader ownership
// explicit to callers.
type SnapshotState interface {
	Reserve(context.Context, string) (*snapshot.Build, error)
	List() ([]*snapshot.Record, error)
	Scan() ([]*snapshot.Record, error)
	IsLeased(string) (bool, error)
	Inspect(string) (*snapshot.Record, error)
	AcquireRead(context.Context, string) (*snapshot.Record, *snapshot.Lease, error)
	LoadManifest(context.Context, string) (*snapshot.Manifest, error)
	Remove(string) (*snapshot.Record, error)
}

// NetworkState is the provider metadata capability. Host device operations
// are deliberately not hidden behind this interface; callers must complete
// provider cleanup before deleting the durable record.
type NetworkState interface {
	List() ([]kbnetwork.Record, error)
	UpsertRecord(kbnetwork.Record) error
	DeleteRecord(string) error
	MarkCleanupPending(string, string) error
	Inspect(string) (*kbnetwork.InspectResult, error)
	InspectVM(string, string, string, []string, []kbnetwork.Config) (*kbnetwork.InspectResult, error)
	ListLeases() (map[string]kbnetwork.Lease, error)
	ReadHostTapState() (*kbnetwork.HostTapState, error)
	IncrementHostTapRef(int) error
	DecrementHostTapRef(int) error
}

var _ ImageState = (*kbimage.Store)(nil)
var _ SnapshotState = (*snapshot.Store)(nil)
var _ NetworkState = (*kbnetwork.Store)(nil)
