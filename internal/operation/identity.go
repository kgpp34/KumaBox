package operation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	idPrefix           = "op_"
	idRandomBytes      = 16
	idEncodedLength    = idRandomBytes * 2
	maxRequestIDLength = 128
	uuidBytes          = 16
)

var (
	// ErrInvalidID identifies a malformed or zero operation ID.
	ErrInvalidID = errors.New("invalid operation ID")
	// ErrInvalidRequestID identifies a malformed or zero idempotency key.
	ErrInvalidRequestID = errors.New("invalid operation request ID")
)

// ID is the immutable identity of an operation. Its zero value is invalid.
type ID struct {
	value string
}

// NewID creates an operation ID using cryptographically secure randomness.
func NewID() (ID, error) {
	random := make([]byte, idRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return ID{}, fmt.Errorf("generate operation ID: %w", err)
	}
	return ID{value: idPrefix + hex.EncodeToString(random)}, nil
}

// ParseID parses a canonical operation ID.
func ParseID(value string) (ID, error) {
	if err := validateID(value); err != nil {
		return ID{}, err
	}
	return ID{value: value}, nil
}

// String returns the canonical operation ID. It returns an empty string for
// the zero value.
func (id ID) String() string {
	return id.value
}

// IsZero reports whether id is the zero value.
func (id ID) IsZero() bool {
	return id.value == ""
}

// Validate checks that id contains a canonical, non-zero operation ID.
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

// RequestID is a caller-controlled idempotency key. Its zero value is invalid.
type RequestID struct {
	value string
}

// NewRequestID creates a random RFC 4122 UUID version 4 request ID.
func NewRequestID() (RequestID, error) {
	random := make([]byte, uuidBytes)
	if _, err := rand.Read(random); err != nil {
		return RequestID{}, fmt.Errorf("generate operation request ID: %w", err)
	}
	random[6] = random[6]&0x0f | 0x40
	random[8] = random[8]&0x3f | 0x80

	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], random[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], random[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], random[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], random[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], random[10:16])

	return RequestID{value: string(encoded)}, nil
}

// ParseRequestID parses a canonical caller-provided idempotency key.
func ParseRequestID(value string) (RequestID, error) {
	if err := validateRequestID(value); err != nil {
		return RequestID{}, err
	}
	return RequestID{value: value}, nil
}

// String returns the canonical request ID. It returns an empty string for the
// zero value.
func (id RequestID) String() string {
	return id.value
}

// IsZero reports whether id is the zero value.
func (id RequestID) IsZero() bool {
	return id.value == ""
}

// Validate checks that id is canonical and non-zero.
func (id RequestID) Validate() error {
	return validateRequestID(id.value)
}

func validateRequestID(value string) error {
	if len(value) == 0 || len(value) > maxRequestIDLength {
		return fmt.Errorf(
			"%w: length must be between 1 and %d bytes",
			ErrInvalidRequestID,
			maxRequestIDLength,
		)
	}

	for index, character := range []byte(value) {
		if isLowerAlphaNumeric(character) {
			continue
		}
		isSeparator := character == '-' || character == '_' || character == '.'
		if isSeparator && index > 0 && index < len(value)-1 {
			continue
		}
		return fmt.Errorf("%w: %q is not canonical", ErrInvalidRequestID, value)
	}
	return nil
}

func isLowerAlphaNumeric(character byte) bool {
	isLower := character >= 'a' && character <= 'z'
	isDigit := character >= '0' && character <= '9'
	return isLower || isDigit
}
