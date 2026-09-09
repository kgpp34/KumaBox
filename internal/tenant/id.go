package tenant

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	idPrefix       = "tenant_"
	generatedBytes = 16
	maxIDSuffixLen = 63
)

// ErrInvalidID identifies a malformed or zero tenant ID.
var ErrInvalidID = errors.New("invalid tenant ID")

// ID is the stable identity of a tenant and its authorization scope.
// Its zero value is invalid.
type ID struct {
	value string
}

// NewID creates a tenant ID using cryptographically secure randomness.
func NewID() (ID, error) {
	random := make([]byte, generatedBytes)
	if _, err := rand.Read(random); err != nil {
		return ID{}, fmt.Errorf("generate tenant ID: %w", err)
	}
	return ID{value: idPrefix + hex.EncodeToString(random)}, nil
}

// ParseID parses a canonical tenant ID.
func ParseID(value string) (ID, error) {
	if err := validateID(value); err != nil {
		return ID{}, err
	}
	return ID{value: value}, nil
}

// String returns the canonical tenant ID. It returns an empty string for the
// zero value.
func (id ID) String() string {
	return id.value
}

// IsZero reports whether id is the zero value.
func (id ID) IsZero() bool {
	return id.value == ""
}

// Validate checks that id contains a canonical, non-zero tenant ID.
func (id ID) Validate() error {
	return validateID(id.value)
}

func validateID(value string) error {
	if !strings.HasPrefix(value, idPrefix) || len(value) <= len(idPrefix) || len(value) > len(idPrefix)+maxIDSuffixLen {
		return fmt.Errorf("%w: must use %q followed by 1 to %d characters", ErrInvalidID, idPrefix, maxIDSuffixLen)
	}

	suffix := value[len(idPrefix):]
	for index, character := range []byte(suffix) {
		if isLowerAlphaNumeric(character) {
			continue
		}
		if character == '-' && index > 0 && index < len(suffix)-1 {
			continue
		}
		return fmt.Errorf("%w: %q is not canonical", ErrInvalidID, value)
	}
	return nil
}

func isLowerAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}
