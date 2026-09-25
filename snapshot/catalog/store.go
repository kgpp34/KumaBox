// Package catalog persists snapshot identities, optional names, and publication
// state. Artifact capture and removal remain in the snapshot and core packages.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

const (
	// CollectionSnapshots stores ready and pending records by immutable ID.
	CollectionSnapshots metadata.Collection = "snapshots"
	// CollectionNames maps optional human-readable names to snapshot IDs.
	CollectionNames metadata.Collection = "snapshot_names"
)

// Collections declares the record sets required by this adapter.
func Collections() []metadata.Collection {
	return []metadata.Collection{CollectionSnapshots, CollectionNames}
}

// Store adapts shared metadata transactions to snapshot persistence.
type Store struct{ store metadata.Store }

// New constructs a snapshot catalog without taking ownership of the engine.
func New(store metadata.Store) *Store { return &Store{store: store} }

type recordData struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	Description      string    `json:"description,omitempty"`
	SandboxID        string    `json:"sandbox_id"`
	SandboxName      string    `json:"sandbox_name"`
	SourceGeneration uint64    `json:"source_generation"`
	ImageDigest      string    `json:"image_digest"`
	VMM              string    `json:"vmm"`
	CPUs             uint32    `json:"cpus"`
	Memory           int64     `json:"memory"`
	Storage          int64     `json:"storage"`
	NICs             int       `json:"nics,omitempty"`
	NetworkName      string    `json:"network_name,omitempty"`
	Size             int64     `json:"size"`
	CreatedAt        time.Time `json:"created_at"`
	Ready            bool      `json:"ready"`
	Deleting         bool      `json:"deleting,omitempty"`
}

type nameData struct {
	ID string `json:"id"`
}

// Reserve atomically holds an ID and optional name before large capture I/O.
func (s *Store) Reserve(ctx context.Context, snapshot types.Snapshot) error {
	if s == nil || s.store == nil {
		return errors.New("snapshot catalog is not configured")
	}
	if err := snapshot.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		if _, exists, err := writer.Get(ctx, CollectionSnapshots, snapshot.ID.String()); err != nil {
			return err
		} else if exists {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, fmt.Errorf("snapshot ID %s already exists", snapshot.ID))
		}
		if snapshot.Name != "" {
			if _, exists, err := writer.Get(ctx, CollectionNames, snapshot.Name); err != nil {
				return err
			} else if exists {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, fmt.Errorf("snapshot name %q already exists", snapshot.Name))
			}
			rawName, err := json.Marshal(nameData{ID: snapshot.ID.String()})
			if err != nil {
				return err
			}
			if err := writer.Put(ctx, CollectionNames, snapshot.Name, rawName); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(encode(snapshot, false))
		if err != nil {
			return err
		}
		return writer.Put(ctx, CollectionSnapshots, snapshot.ID.String(), raw)
	})
	return errdefs.Context(err, "save snapshot", snapshot.Name, "reserve", "choose another snapshot name", false)
}

// Commit publishes size and readiness after artifacts are atomically visible.
func (s *Store) Commit(ctx context.Context, id types.SnapshotID, size int64) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := load(ctx, writer, id)
		if err != nil {
			return err
		}
		if record.Ready {
			result, err = decodeSnapshot(record)
			if err != nil {
				return err
			}
			return nil
		}
		record.Size = size
		result, err = decodeSnapshot(record)
		if err != nil {
			return err
		}
		record.Ready = true
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := writer.Put(ctx, CollectionSnapshots, id.String(), raw); err != nil {
			return err
		}
		return nil
	})
	return result, errdefs.Context(err, "save snapshot", id.String(), "commit", "inspect snapshot storage before retrying", true)
}

// Forget releases a pending reservation during pre-publication compensation.
func (s *Store) Forget(ctx context.Context, id types.SnapshotID) error {
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := load(ctx, writer, id)
		if err != nil {
			return err
		}
		if record.Ready {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("ready snapshot cannot be forgotten"))
		}
		if record.Name != "" {
			if err := writer.Delete(ctx, CollectionNames, record.Name); err != nil {
				return err
			}
		}
		return writer.Delete(ctx, CollectionSnapshots, id.String())
	})
	return err
}

// Resolve returns one ready snapshot by exact name or complete ID.
func (s *Store) Resolve(ctx context.Context, reference string) (types.Snapshot, error) {
	if s == nil || s.store == nil {
		return types.Snapshot{}, errors.New("snapshot catalog is not configured")
	}
	var result types.Snapshot
	err := s.store.View(ctx, func(reader metadata.Reader) error {
		record, err := resolve(ctx, reader, reference)
		if err != nil {
			return err
		}
		if !record.Ready || record.Deleting {
			return notFound(reference)
		}
		result, err = decodeSnapshot(record)
		if err != nil {
			return err
		}
		return nil
	})
	return result, errdefs.Context(err, "resolve snapshot", reference, "metadata", "check the snapshot name or ID", false)
}

// List returns ready snapshots ordered newest first.
func (s *Store) List(ctx context.Context) ([]types.Snapshot, error) {
	var result []types.Snapshot
	err := s.store.View(ctx, func(reader metadata.Reader) error {
		return reader.Scan(ctx, CollectionSnapshots, func(id string, raw []byte) error {
			record, err := decode(raw)
			if err != nil {
				return err
			}
			if record.ID != id {
				return corrupt(errors.New("snapshot record key differs from ID"))
			}
			if record.Ready && !record.Deleting {
				snapshot, err := decodeSnapshot(record)
				if err != nil {
					return err
				}
				result = append(result, snapshot)
			}
			return nil
		})
	})
	slices.SortFunc(result, func(left, right types.Snapshot) int {
		if order := right.CreatedAt.Compare(left.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(left.ID.String(), right.ID.String())
	})
	return result, errdefs.Context(err, "list snapshots", "", "metadata", "inspect snapshot metadata", false)
}

// BeginDelete records durable deletion intent and returns the artifact owner.
func (s *Store) BeginDelete(ctx context.Context, reference string) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := resolve(ctx, writer, reference)
		if err != nil {
			return err
		}
		if !record.Ready {
			return notFound(reference)
		}
		result, err = decodeSnapshot(record)
		if err != nil {
			return err
		}
		if record.Deleting {
			return nil
		}
		record.Deleting = true
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return writer.Put(ctx, CollectionSnapshots, record.ID, raw)
	})
	return result, errdefs.Context(err, "remove snapshot", reference, "mark deleting", "retry snapshot removal", false)
}

// FinalizeDelete releases metadata and the optional name after artifacts are absent.
func (s *Store) FinalizeDelete(ctx context.Context, id types.SnapshotID) error {
	err := s.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := load(ctx, writer, id)
		if err != nil {
			return err
		}
		if !record.Deleting {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("snapshot is not deleting"))
		}
		if record.Name != "" {
			if err := writer.Delete(ctx, CollectionNames, record.Name); err != nil {
				return err
			}
		}
		return writer.Delete(ctx, CollectionSnapshots, id.String())
	})
	return errdefs.Context(err, "remove snapshot", id.String(), "finalize", "retry snapshot removal", true)
}

func resolve(ctx context.Context, reader metadata.Reader, reference string) (recordData, error) {
	if reference == "" {
		return recordData{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SNAPSHOT must not be empty"))
	}
	if raw, exists, err := reader.Get(ctx, CollectionNames, reference); err != nil {
		return recordData{}, err
	} else if exists {
		var name nameData
		if err := json.Unmarshal(raw, &name); err != nil || name.ID == "" {
			return recordData{}, corrupt(errors.New("invalid snapshot name binding"))
		}
		id, err := types.ParseSnapshotID(name.ID)
		if err != nil {
			return recordData{}, corrupt(err)
		}
		return load(ctx, reader, id)
	}
	id, err := types.ParseSnapshotID(reference)
	if err != nil {
		return recordData{}, notFound(reference)
	}
	return load(ctx, reader, id)
}

func load(ctx context.Context, reader metadata.Reader, id types.SnapshotID) (recordData, error) {
	raw, exists, err := reader.Get(ctx, CollectionSnapshots, id.String())
	if err != nil {
		return recordData{}, err
	}
	if !exists {
		return recordData{}, notFound(id.String())
	}
	return decode(raw)
}

func decode(raw []byte) (recordData, error) {
	var record recordData
	if err := json.Unmarshal(raw, &record); err != nil {
		return recordData{}, corrupt(err)
	}
	if _, err := decodeSnapshot(record); err != nil {
		return recordData{}, corrupt(err)
	}
	return record, nil
}

func encode(snapshot types.Snapshot, ready bool) recordData {
	return recordData{
		ID: snapshot.ID.String(), Name: snapshot.Name, Description: snapshot.Description,
		SandboxID: snapshot.SandboxID.String(), SandboxName: snapshot.Config.Name,
		SourceGeneration: snapshot.SourceGeneration,
		ImageDigest:      snapshot.ImageDigest.String(), VMM: string(snapshot.VMM),
		CPUs: snapshot.Config.CPUs, Memory: snapshot.Config.Memory, Storage: snapshot.Config.Storage,
		NICs: snapshot.Config.NICs, NetworkName: snapshot.Config.NetworkName,
		Size: snapshot.Size, CreatedAt: snapshot.CreatedAt.UTC(), Ready: ready,
	}
}

func decodeSnapshot(record recordData) (types.Snapshot, error) {
	id, err := types.ParseSnapshotID(record.ID)
	if err != nil {
		return types.Snapshot{}, err
	}
	sandboxID, err := types.ParseSandboxID(record.SandboxID)
	if err != nil {
		return types.Snapshot{}, err
	}
	digest, err := types.ParseDigest(record.ImageDigest)
	if err != nil {
		return types.Snapshot{}, err
	}
	result := types.Snapshot{
		ID: id, Name: record.Name, Description: record.Description,
		SandboxID: sandboxID, SourceGeneration: record.SourceGeneration,
		ImageDigest: digest, VMM: types.VMMType(record.VMM), Size: record.Size,
		Config: types.SandboxConfig{
			Name: record.SandboxName, CPUs: record.CPUs, Memory: record.Memory, Storage: record.Storage,
			NICs: record.NICs, NetworkName: record.NetworkName,
		},
		CreatedAt: record.CreatedAt.UTC(),
	}
	return result, result.Validate()
}

func notFound(reference string) error {
	return errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("snapshot %q was not found", reference))
}

func corrupt(cause error) error {
	return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, cause)
}
