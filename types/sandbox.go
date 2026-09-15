// Package types defines data contracts shared across KumaBox modules.
// It contains resource models and value objects, not service interfaces,
// persistence encodings, or command presentation types.
package types

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/kumabox/kumabox/errdefs"
)

const (
	// DefaultSandboxCPUs matches the initial Cocoon-compatible sandbox shape.
	DefaultSandboxCPUs uint32 = 2
	// DefaultSandboxMemory is one gibibyte.
	DefaultSandboxMemory int64 = 1 << 30
	// DefaultSandboxStorage is a ten-gibibyte logical sparse COW disk.
	DefaultSandboxStorage int64 = 10 << 30
	// MinSandboxMemory rejects guests too small for the supported boot path.
	MinSandboxMemory int64 = 512 << 20
	// MinSandboxStorage matches the minimum COW capacity accepted by Cocoon.
	MinSandboxStorage int64 = 10 << 30
	// MaxSandboxCPUs bounds conversion to host-native integer APIs and unreasonable shapes.
	MaxSandboxCPUs uint32 = 1024
)

var validSandboxName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// SandboxState records a durable lifecycle fact. Its zero value is invalid so
// omitted metadata cannot be mistaken for a usable sandbox.
type SandboxState string

const (
	// SandboxStateCreating owns the name, image reference, and any partially prepared disk.
	SandboxStateCreating SandboxState = "creating"
	// SandboxStateCreated means persistent resources are ready and have never been started.
	SandboxStateCreated SandboxState = "created"
	// SandboxStateStarting means a start operation owns runtime preparation.
	SandboxStateStarting SandboxState = "starting"
	// SandboxStateRunning means the owned VMM process passed runtime validation.
	SandboxStateRunning SandboxState = "running"
	// SandboxStateStopping means a stop operation is driving the process toward exit.
	SandboxStateStopping SandboxState = "stopping"
	// SandboxStateStopped means a previously started sandbox has exited.
	SandboxStateStopped SandboxState = "stopped"
	// SandboxStateError retains ownership when cleanup or a lifecycle transition is incomplete.
	SandboxStateError SandboxState = "error"
	// SandboxStateDeleting retains image and resource ownership until removal finishes.
	SandboxStateDeleting SandboxState = "deleting"
)

// SandboxID is a canonical lowercase UUIDv4 used for metadata keys and managed paths.
type SandboxID string

// NewSandboxID generates a UUIDv4 from the operating system's cryptographic random source.
func NewSandboxID() (SandboxID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate sandbox ID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return SandboxID(encoded), nil
}

// ParseSandboxID validates the canonical UUIDv4 representation used by managed paths.
func ParseSandboxID(value string) (SandboxID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return "", fmt.Errorf("invalid sandbox ID %q", value)
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 || decoded[8]&0xc0 != 0x80 {
		return "", fmt.Errorf("invalid sandbox ID %q", value)
	}
	for _, char := range value {
		if char >= 'A' && char <= 'F' {
			return "", fmt.Errorf("invalid sandbox ID %q", value)
		}
	}
	return SandboxID(value), nil
}

// String returns the canonical identifier.
func (id SandboxID) String() string { return string(id) }

// SandboxConfig is the immutable resource request stored with a sandbox.
type SandboxConfig struct {
	// Name is the human-readable lookup key and is never used as a path component.
	Name string
	// CPUs is the number of virtual CPUs exposed to the guest.
	CPUs uint32
	// Memory is guest memory in bytes.
	Memory int64
	// Storage is the logical size of the sparse ext4 COW disk in bytes.
	Storage int64
}

// Validate enforces the resource and naming contract before any persistent change.
func (c SandboxConfig) Validate() error {
	if !validSandboxName.MatchString(c.Name) {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("sandbox name %q must match %s", c.Name, validSandboxName))
	}
	if c.CPUs == 0 || c.CPUs > MaxSandboxCPUs {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--cpus must be between 1 and %d", MaxSandboxCPUs))
	}
	if c.Memory < MinSandboxMemory {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--memory must be at least %d bytes", MinSandboxMemory))
	}
	if c.Storage < MinSandboxStorage {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--storage must be at least %d bytes", MinSandboxStorage))
	}
	return nil
}

// SandboxFailure records why an intermediate sandbox still owns resources and needs inspection.
type SandboxFailure struct {
	// Phase locates the failed operation step.
	Phase string
	// Message is diagnostic text for operators and is not a stable error code.
	Message string
}

// Sandbox is the durable resource aggregate guarded by a generation compare-and-swap.
type Sandbox struct {
	// ID is the immutable metadata and filesystem identity.
	ID SandboxID
	// Config is the immutable requested guest shape.
	Config SandboxConfig
	// ImageDigest pins the exact manifest independently of a mutable local alias.
	ImageDigest Digest
	// State controls which operations may consume owned resources.
	State SandboxState
	// Generation increments on every state transition and fences stale operations.
	Generation uint64
	// Failure is present only when SandboxStateError retains incomplete work.
	Failure *SandboxFailure
	// CreatedAt is the first successful identity reservation time.
	CreatedAt time.Time
	// UpdatedAt is the latest committed transition time.
	UpdatedAt time.Time
}

// Validate rejects incomplete sandbox data before adapters persist or return it.
func (s Sandbox) Validate() error {
	if _, err := ParseSandboxID(s.ID.String()); err != nil {
		return err
	}
	if err := s.Config.Validate(); err != nil {
		return err
	}
	if s.ImageDigest.IsZero() || s.Generation == 0 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		return errors.New("sandbox image, generation, and timestamps must be set")
	}
	switch s.State {
	case SandboxStateCreating, SandboxStateCreated, SandboxStateStarting, SandboxStateRunning,
		SandboxStateStopping, SandboxStateStopped, SandboxStateError, SandboxStateDeleting:
	default:
		return fmt.Errorf("invalid sandbox state %q", s.State)
	}
	if (s.State == SandboxStateError) != (s.Failure != nil) {
		return errors.New("sandbox failure must be present only in error state")
	}
	return nil
}
