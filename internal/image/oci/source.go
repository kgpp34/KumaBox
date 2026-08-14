// SPDX-License-Identifier: MIT

// Source acquisition supports local Docker images and remote registries.
package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// SourceRequest describes an OCI image source.
type SourceRequest struct {
	Ref      string
	Platform string
	Source   string
}

// SourceResult contains an acquired OCI image and its resolved metadata.
type SourceResult struct {
	Image    v1.Image
	Resolved *ResolveResult
	Source   string
}

// openSource acquires an OCI image from the requested source. Source "auto" tries
// the local Docker daemon before falling back to a registry.
func openSource(ctx context.Context, req SourceRequest) (*SourceResult, error) {
	source := req.Source
	if source == "" {
		source = "auto"
	}
	switch source {
	case "auto":
		result, err := openDaemon(ctx, req)
		if err == nil {
			return result, nil
		}
		return openRegistry(ctx, req)
	case "daemon":
		return openDaemon(ctx, req)
	case "registry":
		return openRegistry(ctx, req)
	default:
		return nil, fmt.Errorf("OCI_SOURCE_INVALID: source must be one of auto, registry, or daemon")
	}
}

func openRegistry(ctx context.Context, req SourceRequest) (*SourceResult, error) {
	resolved, err := (Resolver{}).Resolve(ctx, req.Ref, req.Platform)
	if err != nil {
		return nil, err
	}
	parsed, err := name.ParseReference(req.Ref)
	if err != nil {
		return nil, fmt.Errorf("OCI_REF_INVALID: %w", err)
	}
	img, err := remote.Image(parsed,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(resolved.Platform.V1()),
	)
	if err != nil {
		return nil, fmt.Errorf("OCI_PULL_FAILED: %w", err)
	}
	return &SourceResult{Image: img, Resolved: resolved, Source: "registry"}, nil
}

func openDaemon(ctx context.Context, req SourceRequest) (*SourceResult, error) {
	platform, err := ParsePlatform(req.Platform)
	if err != nil {
		return nil, err
	}
	parsed, err := name.ParseReference(req.Ref)
	if err != nil {
		return nil, fmt.Errorf("OCI_REF_INVALID: %w", err)
	}
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "image", "save", req.Ref)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("OCI_DAEMON_IMAGE_FAILED: docker image save %s: %w: %s", req.Ref, err, strings.TrimSpace(stderr.String()))
	}
	img, err := tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(out.Bytes())), nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("OCI_DAEMON_IMAGE_FAILED: parse docker image tar: %w", err)
	}
	if err := validatePlatform(img, platform); err != nil {
		return nil, err
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
	resolved := &ResolveResult{
		Ref:            req.Ref,
		Repository:     parsed.Context().String(),
		ResolvedDigest: digest.String(),
		DigestRef:      parsed.Context().String() + "@" + digest.String(),
		Platform:       platform,
		Config: Descriptor{
			Digest:    manifest.Config.Digest.String(),
			MediaType: string(manifest.Config.MediaType),
			SizeBytes: manifest.Config.Size,
		},
		Layers:     layers,
		ResolvedAt: time.Now().UTC(),
	}
	return &SourceResult{Image: img, Resolved: resolved, Source: "daemon"}, nil
}

func validatePlatform(img v1.Image, want Platform) error {
	raw, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("OCI_CONFIG_FAILED: %w", err)
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("OCI_CONFIG_FAILED: decode daemon image config: %w", err)
	}
	if config.OS != "" && config.OS != want.OS {
		return fmt.Errorf("OCI_PLATFORM_MISMATCH: daemon image os=%s, requested=%s", config.OS, want.OS)
	}
	if config.Architecture != "" && config.Architecture != want.Architecture {
		return fmt.Errorf("OCI_PLATFORM_MISMATCH: daemon image architecture=%s, requested=%s", config.Architecture, want.Architecture)
	}
	if want.Variant != "" && config.Variant != "" && config.Variant != want.Variant {
		return fmt.Errorf("OCI_PLATFORM_MISMATCH: daemon image variant=%s, requested=%s", config.Variant, want.Variant)
	}
	return nil
}
