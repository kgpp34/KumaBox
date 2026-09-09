package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	idPrefix        = "kb_"
	idRandomBytes   = 16
	idEncodedLength = idRandomBytes * 2
)

// ErrInvalidID identifies a malformed or zero sandbox ID.
var ErrInvalidID = errors.New("invalid sandbox ID")

// ID is the immutable identity of a sandbox. Its zero value is invalid.
type ID struct {
	value string
}

// NewID creates a sandbox ID using cryptographically secure randomness.
func NewID() (ID, error) {
	random := make([]byte, idRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return ID{}, fmt.Errorf("generate sandbox ID: %w", err)
	}
	return ID{value: idPrefix + hex.EncodeToString(random)}, nil
}

// ParseID parses a canonical sandbox ID.
func ParseID(value string) (ID, error) {
	if err := validateID(value); err != nil {
		return ID{}, err
	}
	return ID{value: value}, nil
}

// String returns the canonical sandbox ID. It returns an empty string for the
// zero value.
func (id ID) String() string {
	return id.value
}

// IsZero reports whether id is the zero value.
func (id ID) IsZero() bool {
	return id.value == ""
}

// Validate checks that id contains a canonical, non-zero sandbox ID.
func (id ID) Validate() error {
	return validateID(id.value)
}

func validateID(value string) error {
	if !strings.HasPrefix(value, idPrefix) || len(value) != len(idPrefix)+idEncodedLength {
		return fmt.Errorf("%w: must use %q followed by %d lowercase hexadecimal characters", ErrInvalidID, idPrefix, idEncodedLength)
	}

	encoded := value[len(idPrefix):]
	if encoded != strings.ToLower(encoded) {
		return fmt.Errorf("%w: hexadecimal encoding must be lowercase", ErrInvalidID)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("%w: decode identity: %v", ErrInvalidID, err)
	}
	return nil
}
