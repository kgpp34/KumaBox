package sandbox

import (
	"errors"
	"fmt"
)

const maxNameLength = 63

// ErrInvalidName identifies a malformed or zero sandbox name.
var ErrInvalidName = errors.New("invalid sandbox name")

// Name is a canonical, human-selected sandbox name. Its zero value is invalid.
type Name struct {
	value string
}

// ParseName parses a lowercase DNS-label-shaped sandbox name.
func ParseName(value string) (Name, error) {
	if err := validateName(value); err != nil {
		return Name{}, err
	}
	return Name{value: value}, nil
}

// String returns the canonical name. It returns an empty string for the zero
// value.
func (name Name) String() string {
	return name.value
}

// IsZero reports whether name is the zero value.
func (name Name) IsZero() bool {
	return name.value == ""
}

// Validate checks that name is canonical and non-zero.
func (name Name) Validate() error {
	return validateName(name.value)
}

func validateName(value string) error {
	if len(value) == 0 || len(value) > maxNameLength {
		return fmt.Errorf("%w: length must be between 1 and %d bytes", ErrInvalidName, maxNameLength)
	}
	for index, character := range []byte(value) {
		if isLowerAlphaNumeric(character) {
			continue
		}
		if character == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return fmt.Errorf("%w: %q is not a lowercase DNS label", ErrInvalidName, value)
	}
	return nil
}

func isLowerAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}
