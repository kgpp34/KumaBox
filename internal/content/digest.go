package content

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	sha256Algorithm = "sha256"
	sha256Prefix    = sha256Algorithm + ":"
	sha256HexLength = sha256.Size * 2
)

// ErrInvalidDigest identifies a malformed, unsupported, or zero digest.
var ErrInvalidDigest = errors.New("invalid content digest")

// Digest is the canonical identity of immutable content. A Digest currently
// supports SHA-256 only. Its zero value is invalid.
type Digest struct {
	value string
}

// SHA256 returns the digest of data.
func SHA256(data []byte) Digest {
	sum := sha256.Sum256(data)
	return Digest{value: sha256Prefix + hex.EncodeToString(sum[:])}
}

// ParseDigest parses a canonical SHA-256 digest.
func ParseDigest(value string) (Digest, error) {
	if err := validateDigest(value); err != nil {
		return Digest{}, err
	}
	return Digest{value: value}, nil
}

// String returns the canonical digest. It returns an empty string for the zero
// value.
func (digest Digest) String() string {
	return digest.value
}

// IsZero reports whether digest is the zero value.
func (digest Digest) IsZero() bool {
	return digest.value == ""
}

// Algorithm returns the digest algorithm, or an empty string for the zero
// value.
func (digest Digest) Algorithm() string {
	if digest.IsZero() {
		return ""
	}
	return sha256Algorithm
}

// Encoded returns the lowercase hexadecimal digest, or an empty string for the
// zero value.
func (digest Digest) Encoded() string {
	if digest.IsZero() {
		return ""
	}
	return digest.value[len(sha256Prefix):]
}

// Validate checks that digest is canonical and supported.
func (digest Digest) Validate() error {
	return validateDigest(digest.value)
}

func validateDigest(value string) error {
	if !strings.HasPrefix(value, sha256Prefix) {
		return fmt.Errorf("%w: only %s is supported", ErrInvalidDigest, sha256Algorithm)
	}

	encoded := value[len(sha256Prefix):]
	if len(encoded) != sha256HexLength {
		return fmt.Errorf("%w: %s must contain %d hexadecimal characters", ErrInvalidDigest, sha256Algorithm, sha256HexLength)
	}
	if encoded != strings.ToLower(encoded) {
		return fmt.Errorf("%w: hexadecimal encoding must be lowercase", ErrInvalidDigest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrInvalidDigest, sha256Algorithm, err)
	}
	return nil
}
