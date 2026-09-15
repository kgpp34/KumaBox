// Package catalog persists sandbox records, names, and image usage in shared metadata.
// It owns encoding and transaction rules; application orchestration and disk I/O
// remain in their dedicated packages.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

const (
	// CollectionSandboxes stores one aggregate per immutable sandbox ID.
	CollectionSandboxes metadata.Collection = "sandboxes"
	// CollectionNames maps a user-facing name to one sandbox ID.
	CollectionNames metadata.Collection = "sandbox_names"
)

// Collections declares the record sets required by this adapter.
func Collections() []metadata.Collection {
	return []metadata.Collection{CollectionSandboxes, CollectionNames}
}

// ImageReader resolves an image inside the caller's metadata transaction.
// Implementations must use reader directly and must not open a nested transaction.
type ImageReader interface {
	Resolve(context.Context, metadata.Reader, string) (types.Image, error)
}

// Store adapts a shared metadata engine to sandbox persistence operations.
// It neither owns the engine nor modifies sandbox files.
type Store struct {
	// store supplies atomic writes spanning sandbox and image collections.
	store metadata.Store
	// images rechecks an image binding within the reservation transaction.
	images ImageReader
}

// New constructs a sandbox catalog over an existing shared store.
func New(store metadata.Store, imageReader ImageReader) *Store {
	return &Store{store: store, images: imageReader}
}

// recordData is the stable adapter-owned JSON representation of a sandbox aggregate.
type recordData struct {
	// ID must equal the CollectionSandboxes key.
	ID string `json:"id"`
	// Name is the immutable user-facing sandbox name.
	Name string `json:"name"`
	// CPUs is the requested virtual CPU count.
	CPUs uint32 `json:"cpus"`
	// Memory is guest memory in bytes.
	Memory int64 `json:"memory"`
	// Storage is logical COW capacity in bytes.
	Storage int64 `json:"storage"`
	// ImageDigest pins the canonical manifest record.
	ImageDigest string `json:"image_digest"`
	// State is explicitly mapped back into the domain enum.
	State string `json:"state"`
	// Generation fences stale state transitions.
	Generation uint64 `json:"generation"`
	// Failure retains incomplete cleanup diagnostics only in Error state.
	Failure *failureData `json:"failure,omitempty"`
	// CreatedAt records initial reservation time.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt records the latest transition time.
	UpdatedAt time.Time `json:"updated_at"`
}

// failureData keeps diagnostic operation failure facts out of stable error codes.
type failureData struct {
	// Phase locates the failed operation step.
	Phase string `json:"phase"`
	// Message preserves operator diagnostics without becoming a stable code.
	Message string `json:"message"`
}

// nameData is deliberately small so names can be checked without decoding aggregates.
type nameData struct {
	// ID is the owner in CollectionSandboxes.
	ID string `json:"id"`
}

// Reserve atomically rechecks the image, claims the name, and writes a Creating record.
// expected protects against an alias rebound while image artifact locks were acquired.
func (c *Store) Reserve(ctx context.Context, imageReference string, expected types.Digest, record types.Sandbox) error {
	if c == nil || c.store == nil || c.images == nil {
		return errors.New("sandbox catalog is not configured")
	}
	if err := record.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if record.State != types.SandboxStateCreating || record.Generation != 1 || record.ImageDigest != expected {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("reservation must be generation-one Creating state for the expected image"))
	}
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		image, err := c.images.Resolve(ctx, writer, imageReference)
		if err != nil {
			return err
		}
		if image.ManifestDigest != expected {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("image binding changed while reserving sandbox; retry"))
		}
		if _, exists, err := writer.Get(ctx, CollectionSandboxes, record.ID.String()); err != nil {
			return err
		} else if exists {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox ID %s already exists", record.ID))
		}
		if raw, exists, err := writer.Get(ctx, CollectionNames, record.Config.Name); err != nil {
			return err
		} else if exists {
			var current nameData
			if err := json.Unmarshal(raw, &current); err != nil {
				return corrupt("sandbox name", err)
			}
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, fmt.Errorf("sandbox name %q is already used by %s", record.Config.Name, current.ID))
		}
		if err := putJSON(ctx, writer, CollectionSandboxes, record.ID.String(), encode(record)); err != nil {
			return err
		}
		return putJSON(ctx, writer, CollectionNames, record.Config.Name, nameData{ID: record.ID.String()})
	})
	return errdefs.Context(err, "reserve sandbox", record.Config.Name, "metadata", "choose another name or retry", false)
}

// MarkCreated performs the create commit only when state and generation still match.
func (c *Store) MarkCreated(ctx context.Context, id types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	return c.transition(ctx, id, expected, types.SandboxStateCreating, types.SandboxStateCreated, nil, updated)
}

// MarkError retains ownership and diagnostics when create cleanup cannot finish.
func (c *Store) MarkError(ctx context.Context, id types.SandboxID, expected uint64, failure types.SandboxFailure, updated time.Time) (types.Sandbox, error) {
	if failure.Phase == "" || failure.Message == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("error transition requires phase and message"))
	}
	return c.transition(ctx, id, expected, types.SandboxStateCreating, types.SandboxStateError, &failure, updated)
}

// transition applies one generation-fenced state change and returns the committed record.
func (c *Store) transition(ctx context.Context, id types.SandboxID, expected uint64, from, to types.SandboxState, failure *types.SandboxFailure, updated time.Time) (types.Sandbox, error) {
	var result types.Sandbox
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := load(ctx, writer, id)
		if err != nil {
			return err
		}
		if record.Generation != expected || record.State != from {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s changed from expected %s generation %d", id, from, expected))
		}
		record.State = to
		record.Generation++
		record.Failure = failure
		record.UpdatedAt = updated
		if err := record.Validate(); err != nil {
			return corrupt("sandbox transition", err)
		}
		if err := putJSON(ctx, writer, CollectionSandboxes, id.String(), encode(record)); err != nil {
			return err
		}
		result = record
		return nil
	})
	return result, errdefs.Context(err, "transition sandbox", id.String(), "metadata", "inspect the sandbox state", false)
}

// Forget removes a failed Creating reservation only if its generation is unchanged.
// The caller must prove that every resource owned by the record was removed first.
func (c *Store) Forget(ctx context.Context, id types.SandboxID, expected uint64) error {
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		record, err := load(ctx, writer, id)
		if err != nil {
			return err
		}
		if record.Generation != expected || record.State != types.SandboxStateCreating {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is no longer the expected Creating reservation", id))
		}
		raw, exists, err := writer.Get(ctx, CollectionNames, record.Config.Name)
		if err != nil {
			return err
		}
		if !exists {
			return corrupt("sandbox name", errors.New("name binding is missing"))
		}
		var name nameData
		if err := json.Unmarshal(raw, &name); err != nil {
			return corrupt("sandbox name", err)
		}
		if name.ID != id.String() {
			return corrupt("sandbox name", errors.New("name binding points to another sandbox"))
		}
		if err := writer.Delete(ctx, CollectionNames, record.Config.Name); err != nil {
			return err
		}
		return writer.Delete(ctx, CollectionSandboxes, id.String())
	})
	return errdefs.Context(err, "forget sandbox", id.String(), "metadata", "inspect the retained sandbox record", false)
}

// Usage answers image deletion from the same metadata transaction that removes
// the last image alias. Every retained sandbox record is a live reference.
type Usage struct{}

// InUse reports whether any sandbox pins digest without opening another transaction.
func (Usage) InUse(ctx context.Context, reader metadata.Reader, digest types.Digest) (bool, error) {
	used := false
	err := reader.Scan(ctx, CollectionSandboxes, func(_ string, raw []byte) error {
		record, err := decode(raw)
		if err != nil {
			return err
		}
		if record.ImageDigest == digest {
			used = true
		}
		return nil
	})
	return used, err
}

// load fetches and validates one sandbox aggregate inside the caller's transaction.
func load(ctx context.Context, reader metadata.Reader, id types.SandboxID) (types.Sandbox, error) {
	raw, exists, err := reader.Get(ctx, CollectionSandboxes, id.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	if !exists {
		return types.Sandbox{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("sandbox %s not found", id))
	}
	record, err := decode(raw)
	if err != nil {
		return types.Sandbox{}, err
	}
	if record.ID != id {
		return types.Sandbox{}, corrupt("sandbox ID", errors.New("record key differs from stored ID"))
	}
	return record, nil
}

// encode maps the domain aggregate to stable adapter-owned storage fields.
func encode(record types.Sandbox) recordData {
	data := recordData{
		ID: record.ID.String(), Name: record.Config.Name, CPUs: record.Config.CPUs,
		Memory: record.Config.Memory, Storage: record.Config.Storage,
		ImageDigest: record.ImageDigest.String(), State: string(record.State),
		Generation: record.Generation, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if record.Failure != nil {
		data.Failure = &failureData{Phase: record.Failure.Phase, Message: record.Failure.Message}
	}
	return data
}

// decode validates persisted JSON before exposing it to application consumers.
func decode(raw []byte) (types.Sandbox, error) {
	var data recordData
	if err := json.Unmarshal(raw, &data); err != nil {
		return types.Sandbox{}, corrupt("sandbox", err)
	}
	id, err := types.ParseSandboxID(data.ID)
	if err != nil {
		return types.Sandbox{}, corrupt("sandbox ID", err)
	}
	digest, err := types.ParseDigest(data.ImageDigest)
	if err != nil {
		return types.Sandbox{}, corrupt("sandbox image", err)
	}
	record := types.Sandbox{
		ID: id, Config: types.SandboxConfig{Name: data.Name, CPUs: data.CPUs, Memory: data.Memory, Storage: data.Storage},
		ImageDigest: digest, State: types.SandboxState(data.State), Generation: data.Generation,
		CreatedAt: data.CreatedAt, UpdatedAt: data.UpdatedAt,
	}
	if data.Failure != nil {
		record.Failure = &types.SandboxFailure{Phase: data.Failure.Phase, Message: data.Failure.Message}
	}
	if err := record.Validate(); err != nil {
		return types.Sandbox{}, corrupt("sandbox", err)
	}
	return record, nil
}

// putJSON keeps all record writes consistently encoded.
func putJSON(ctx context.Context, writer metadata.Writer, collection metadata.Collection, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writer.Put(ctx, collection, key, raw)
}

// corrupt classifies malformed persisted data independently of caller operations.
func corrupt(entity string, cause error) error {
	return errdefs.Context(errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, cause), "read sandbox metadata", entity, "decode", "restore metadata from a trusted backup", false)
}
