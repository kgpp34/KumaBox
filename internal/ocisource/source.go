// SPDX-License-Identifier: MIT

// Package ocisource acquires OCI images from supported local and remote
// sources. It does not persist manifests or blobs.
package ocisource

import (
	"bytes"
	"context"
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

	"github.com/kumabox/kumabox/internal/ociresolver"
)

// Request describes an OCI image source.
type Request struct {
	Ref      string
	Platform string
	Source   string
}

// Result contains an acquired OCI image and its resolved metadata.
type Result struct {
	Image    v1.Image
	Resolved *ociresolver.Result
	Source   string
}

// Open acquires an OCI image from the requested source. Source "auto" tries
// the local Docker daemon before falling back to a registry.
func Open(ctx context.Context, req Request) (*Result, error) {
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

func openRegistry(ctx context.Context, req Request) (*Result, error) {
	resolved, err := (ociresolver.Resolver{}).Resolve(ctx, req.Ref, req.Platform)
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
	return &Result{Image: img, Resolved: resolved, Source: "registry"}, nil
}

func openDaemon(ctx context.Context, req Request) (*Result, error) {
	platform, err := ociresolver.ParsePlatform(req.Platform)
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
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("OCI_DIGEST_FAILED: %w", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("OCI_MANIFEST_FAILED: %w", err)
	}
	layers := make([]ociresolver.Descriptor, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		layers = append(layers, ociresolver.Descriptor{
			Digest:    layer.Digest.String(),
			MediaType: string(layer.MediaType),
			SizeBytes: layer.Size,
		})
	}
	resolved := &ociresolver.Result{
		Ref:            req.Ref,
		Repository:     parsed.Context().String(),
		ResolvedDigest: digest.String(),
		DigestRef:      parsed.Context().String() + "@" + digest.String(),
		Platform:       platform,
		Config: ociresolver.Descriptor{
			Digest:    manifest.Config.Digest.String(),
			MediaType: string(manifest.Config.MediaType),
			SizeBytes: manifest.Config.Size,
		},
		Layers:     layers,
		ResolvedAt: time.Now().UTC(),
	}
	return &Result{Image: img, Resolved: resolved, Source: "daemon"}, nil
}
