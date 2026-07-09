// SPDX-License-Identifier: MIT

package ociresolver

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Platform identifies the OCI platform selected from a manifest list.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// Descriptor describes one OCI descriptor needed by later content-store steps.
type Descriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Result is the digest-pinned view of an OCI image reference.
type Result struct {
	Ref            string       `json:"ref"`
	Repository     string       `json:"repository"`
	ResolvedDigest string       `json:"resolvedDigest"`
	DigestRef      string       `json:"digestRef"`
	Platform       Platform     `json:"platform"`
	Config         Descriptor   `json:"config"`
	Layers         []Descriptor `json:"layers"`
	ResolvedAt     time.Time    `json:"resolvedAt"`
}

// Resolver resolves OCI refs using a registry, auth keychain, and selected platform.
type Resolver struct{}

// Resolve resolves ref to a single image manifest and returns its pinned digest.
func (Resolver) Resolve(ctx context.Context, ref string, platform string) (*Result, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("OCI_REF_INVALID: %w", err)
	}
	selected, err := ParsePlatform(platform)
	if err != nil {
		return nil, err
	}

	img, err := remote.Image(parsed,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{
			OS:           selected.OS,
			Architecture: selected.Architecture,
			Variant:      selected.Variant,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("OCI_RESOLVE_FAILED: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("OCI_DIGEST_FAILED: %w", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("OCI_MANIFEST_FAILED: %w", err)
	}

	layers := make([]Descriptor, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		layers = append(layers, Descriptor{
			Digest:    layer.Digest.String(),
			MediaType: string(layer.MediaType),
			SizeBytes: layer.Size,
		})
	}

	return &Result{
		Ref:            parsed.String(),
		Repository:     parsed.Context().String(),
		ResolvedDigest: digest.String(),
		DigestRef:      parsed.Context().String() + "@" + digest.String(),
		Platform:       selected,
		Config: Descriptor{
			Digest:    manifest.Config.Digest.String(),
			MediaType: string(manifest.Config.MediaType),
			SizeBytes: manifest.Config.Size,
		},
		Layers:     layers,
		ResolvedAt: time.Now().UTC(),
	}, nil
}

// DefaultPlatform returns the host Linux OCI platform used when no flag is set.
func DefaultPlatform() string {
	return "linux/" + runtime.GOARCH
}

// ParsePlatform parses os/arch[/variant] strings.
func ParsePlatform(value string) (Platform, error) {
	if value == "" {
		value = DefaultPlatform()
	}
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, fmt.Errorf("PLATFORM_INVALID: platform must be os/arch or os/arch/variant, got %q", value)
	}
	if parts[0] != "linux" {
		return Platform{}, fmt.Errorf("PLATFORM_UNSUPPORTED: only linux OCI images are supported, got %q", parts[0])
	}
	return Platform{
		OS:           parts[0],
		Architecture: parts[1],
		Variant:      variant(parts),
	}, nil
}

func variant(parts []string) string {
	if len(parts) == 3 {
		return parts[2]
	}
	return ""
}
