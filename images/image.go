package images

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Digest is a validated SHA-256 content identity.
type Digest struct {
	value [32]byte
}

func ParseDigest(value string) (Digest, error) {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 || value != strings.ToLower(value) {
		return Digest{}, fmt.Errorf("invalid sha256 digest %q", value)
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(hexValue)
	if err != nil || len(decoded) != 32 {
		return Digest{}, fmt.Errorf("invalid sha256 digest %q", value)
	}
	var digest Digest
	copy(digest.value[:], decoded)
	return digest, nil
}

func (d Digest) String() string { return "sha256:" + hex.EncodeToString(d.value[:]) }
func (d Digest) Hex() string    { return hex.EncodeToString(d.value[:]) }
func (d Digest) IsZero() bool   { return d == Digest{} }

func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Digest) UnmarshalText(value []byte) error {
	parsed, err := ParseDigest(string(value))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

type Platform struct {
	OS           string
	Architecture string
}

type Layer struct {
	SourceDigest Digest
	EROFSDigest  Digest
	Size         int64
	BootFiles    []BootFile
	Whiteouts    []string
	BootOpaque   bool
}

// BootFile is a regular boot candidate extracted from a source layer.
type BootFile struct {
	Name   string
	Digest Digest
	Size   int64
}

type Boot struct {
	KernelFile  string
	InitrdFile  string
	KernelLayer Digest
	InitrdLayer Digest
}

type Image struct {
	Names          []string
	ManifestDigest Digest
	Platform       Platform
	Layers         []Layer
	Boot           Boot
	Size           int64
	CreatedAt      time.Time
}

type Manifest struct {
	Digest   Digest
	Platform Platform
	Layers   []Descriptor
}

type Descriptor struct {
	Digest Digest
	Size   int64
}

type ImportCommit struct {
	Name     string
	Manifest Manifest
	Layers   []Layer
	Boot     Boot
	Size     int64
	Created  time.Time
}

type Removal struct {
	Names  []string
	Layers []Digest
}
