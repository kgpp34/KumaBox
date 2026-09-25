package types

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

var validSnapshotName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,62}$`)

// SnapshotID is the immutable UUIDv4 identity of one captured sandbox state.
type SnapshotID string

// NewSnapshotID generates a snapshot identity from the same cryptographic UUID
// source used for sandboxes.
func NewSnapshotID() (SnapshotID, error) {
	id, err := NewSandboxID()
	return SnapshotID(id), err
}

// ParseSnapshotID validates the canonical UUIDv4 representation.
func ParseSnapshotID(value string) (SnapshotID, error) {
	if _, err := ParseSandboxID(value); err != nil {
		return "", fmt.Errorf("invalid snapshot ID %q", value)
	}
	return SnapshotID(value), nil
}

// String returns the canonical snapshot identifier.
func (id SnapshotID) String() string { return string(id) }

// Snapshot is the durable description of one complete VMM and writable-disk
// capture. Immutable image layers remain pinned by ImageDigest.
type Snapshot struct {
	// ID is the immutable metadata and artifact directory identity.
	ID SnapshotID
	// Name is an optional human-readable lookup key.
	Name string
	// Description is optional operator context.
	Description string
	// SandboxID identifies the source lineage accepted by restore.
	SandboxID SandboxID
	// SourceGeneration is the Running generation captured by this snapshot.
	SourceGeneration uint64
	// ImageDigest pins the immutable image layers required by the sandbox.
	ImageDigest Digest
	// VMM selects the adapter capable of restoring the native snapshot.
	VMM VMMType
	// Config is the source sandbox resource and network request.
	Config SandboxConfig
	// Size is the allocated snapshot artifact size in bytes.
	Size int64
	// CreatedAt records when capture was requested.
	CreatedAt time.Time
}

// Validate rejects snapshot facts that cannot safely drive lookup or restore.
func (s Snapshot) Validate() error {
	if _, err := ParseSnapshotID(s.ID.String()); err != nil {
		return err
	}
	if s.Name != "" && !validSnapshotName.MatchString(s.Name) {
		return fmt.Errorf("snapshot name %q must match %s", s.Name, validSnapshotName)
	}
	if _, err := ParseSandboxID(s.SandboxID.String()); err != nil {
		return err
	}
	if s.SourceGeneration == 0 {
		return errors.New("snapshot source generation must be positive")
	}
	if _, err := ParseDigest(s.ImageDigest.String()); err != nil {
		return err
	}
	if err := s.VMM.Validate(); err != nil {
		return err
	}
	if err := s.Config.Validate(); err != nil {
		return err
	}
	if s.Size < 0 || s.CreatedAt.IsZero() {
		return errors.New("snapshot size must be non-negative and creation time must be set")
	}
	return nil
}
