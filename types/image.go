package types

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Digest is a validated SHA-256 content identity.
type Digest struct {
	// value prevents constructing malformed textual identities outside this package.
	value [32]byte
}

// ParseDigest accepts only canonical lowercase sha256:<64 hex digits> identities.
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

// String returns the canonical algorithm-prefixed identity.
func (d Digest) String() string { return "sha256:" + hex.EncodeToString(d.value[:]) }

// Hex returns the hexadecimal identity used in managed artifact filenames.
func (d Digest) Hex() string { return hex.EncodeToString(d.value[:]) }

// IsZero reports the unset identity, which image metadata must not use.
func (d Digest) IsZero() bool { return d == Digest{} }

// MarshalText encodes the canonical identity for text and JSON serialization.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText validates an identity before replacing the receiver.
func (d *Digest) UnmarshalText(value []byte) error {
	parsed, err := ParseDigest(string(value))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Platform selects the operating system and instruction set of an image.
type Platform struct {
	// OS is the source operating system; KumaBox currently accepts linux.
	OS string
	// Architecture is the source instruction set: amd64 or arm64.
	Architecture string
}

// Layer describes one converted artifact and its boot overlay effects.
// Layers remain in source manifest order, from the base to the top layer.
type Layer struct {
	// SourceDigest identifies the original source blob and keys shared artifacts.
	SourceDigest Digest
	// EROFSDigest verifies the converted filesystem, not the source blob.
	EROFSDigest Digest
	// Size is the converted EROFS size in bytes.
	Size int64
	// BootFiles contains extracted regular kernel and initrd candidates.
	BootFiles []BootFile
	// Whiteouts names boot candidates hidden from lower layers.
	Whiteouts []string
	// BootOpaque hides all boot candidates inherited from lower layers.
	BootOpaque bool
}

// BootFile is a regular boot candidate extracted from a source layer.
type BootFile struct {
	// Name is an accepted /boot basename, without parent directories.
	Name string
	// Digest verifies the extracted artifact, including any kernel decompression.
	Digest Digest
	// Size is the extracted artifact size in bytes and must be positive.
	Size int64
}

// Boot identifies the surviving kernel and initrd selected across all layers.
type Boot struct {
	// KernelFile is the selected kernel basename within its layer's boot directory.
	KernelFile string
	// InitrdFile is the selected initrd basename within its layer's boot directory.
	InitrdFile string
	// KernelLayer identifies the source layer that supplied KernelFile.
	KernelLayer Digest
	// InitrdLayer identifies the source layer that supplied InitrdFile.
	InitrdLayer Digest
}

// Image is a committed manifest together with its local names and artifacts.
type Image struct {
	// Names contains local aliases bound to the manifest, sorted by the catalog.
	Names []string
	// ManifestDigest identifies the resolved source manifest or its normalized form.
	ManifestDigest Digest
	// Platform is the operating system and instruction set of all layers.
	Platform Platform
	// Layers preserves manifest order, including repeated source layers.
	Layers []Layer
	// Boot records the kernel and initrd selected after applying overlay rules.
	Boot Boot
	// Size sums the EROFS sizes in Layers, including repeated occurrences.
	Size int64
	// CreatedAt is the initial local import time, not the source image build time.
	CreatedAt time.Time
}

// Manifest is the format-independent result of resolving a source for a platform.
type Manifest struct {
	// Digest identifies this manifest; archive adapters may synthesize it.
	Digest Digest
	// Platform must match the platform requested from Source.Resolve.
	Platform Platform
	// Layers lists original source blobs in filesystem overlay order.
	Layers []Descriptor
}

// Descriptor identifies a layer blob before decompression or conversion.
type Descriptor struct {
	// Digest identifies the source bytes and remains the converted artifact's key.
	Digest Digest
	// Size is the source blob size in bytes, not its unpacked or EROFS size.
	Size int64
}

// Valid reports whether the platform is supported by KumaBox.
func (p Platform) Valid() bool {
	return p.OS == "linux" && (p.Architecture == "amd64" || p.Architecture == "arm64")
}

// Equal compares the content and boot metadata of two layer artifacts.
func (a Layer) Equal(b Layer) bool {
	return a.SourceDigest == b.SourceDigest && a.EROFSDigest == b.EROFSDigest && a.Size == b.Size && a.BootOpaque == b.BootOpaque && slices.Equal(a.BootFiles, b.BootFiles) && slices.Equal(a.Whiteouts, b.Whiteouts)
}
