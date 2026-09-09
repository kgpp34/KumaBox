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
	maxNameLength   = 63
)

var (
	// ErrInvalidID identifies a malformed or zero sandbox ID.
	ErrInvalidID = errors.New("invalid sandbox ID")
	// ErrInvalidName identifies a malformed or zero sandbox name.
	ErrInvalidName = errors.New("invalid sandbox name")
)

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
	validLength := len(value) == len(idPrefix)+idEncodedLength
	if !strings.HasPrefix(value, idPrefix) || !validLength {
		return fmt.Errorf(
			"%w: must use %q followed by %d lowercase hexadecimal characters",
			ErrInvalidID,
			idPrefix,
			idEncodedLength,
		)
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

// Name is a human-selected sandbox name. It follows grammar:
// an ASCII letter or digit followed by at most 62 letters, digits, dots,
// underscores, or hyphens. Its zero value is invalid.
type Name struct {
	value string
}

// ParseName parses a sandbox name.
func ParseName(value string) (Name, error) {
	if err := validateName(value); err != nil {
		return Name{}, err
	}
	return Name{value: value}, nil
}

// String returns the name. It returns an empty string for the zero value.
func (name Name) String() string {
	return name.value
}

// IsZero reports whether name is the zero value.
func (name Name) IsZero() bool {
	return name.value == ""
}

// Validate checks that name is non-zero and follows the sandbox name grammar.
func (name Name) Validate() error {
	return validateName(name.value)
}

func validateName(value string) error {
	if len(value) == 0 || len(value) > maxNameLength {
		return fmt.Errorf(
			"%w: length must be between 1 and %d bytes",
			ErrInvalidName,
			maxNameLength,
		)
	}
	if !isASCIIAlphaNumeric(value[0]) {
		return fmt.Errorf("%w: %q must start with an ASCII letter or digit", ErrInvalidName, value)
	}
	for _, character := range []byte(value[1:]) {
		if isASCIIAlphaNumeric(character) || character == '.' || character == '_' || character == '-' {
			continue
		}
		return fmt.Errorf("%w: %q contains an unsupported character", ErrInvalidName, value)
	}
	return nil
}

func isASCIIAlphaNumeric(character byte) bool {
	isLower := character >= 'a' && character <= 'z'
	isUpper := character >= 'A' && character <= 'Z'
	isDigit := character >= '0' && character <= '9'
	return isLower || isUpper || isDigit
}
