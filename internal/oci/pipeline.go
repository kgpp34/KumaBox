// SPDX-License-Identifier: MIT

// Package oci coordinates OCI reference resolution, content retrieval, and
// publication as a bootable KumaBox image.
package oci

import (
	"context"

	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/ocibuild"
	"github.com/kumabox/kumabox/internal/ociresolver"
	"github.com/kumabox/kumabox/internal/ocistore"
)

// The aliases below form the OCI workflow contract exposed to callers while
// the lower-level packages retain ownership of their persistence models.
type (
	BuildRequest  = ocibuild.BuildRequest
	ProgressEvent = ocistore.ProgressEvent
	PullRequest   = ocistore.PullRequest
	PullResult    = ocistore.PullResult
	ResolveResult = ociresolver.Result
)

// Content retrieves and indexes OCI content.
type Content interface {
	Pull(context.Context, ocistore.PullRequest) (*ocistore.PullResult, error)
}

// ImageCatalog publishes durable managed-image records.
type ImageCatalog interface {
	Create(image.CreateRequest) (*image.ImageRecord, error)
}

type imageBuilder interface {
	Build(context.Context, ocibuild.BuildRequest) (*image.ImageRecord, error)
}

// ImagePipeline presents one entry point for all OCI-backed image workflows.
type ImagePipeline struct {
	content Content
	builder imageBuilder
}

// NewImagePipeline creates an OCI image pipeline using caller-owned metadata
// capabilities. The pipeline does not own or close those capabilities.
func NewImagePipeline(rootDir string, content Content, images ImageCatalog) *ImagePipeline {
	return &ImagePipeline{
		content: content,
		builder: ocibuild.NewWithStores(rootDir, content, images),
	}
}

// DefaultPlatform returns the host Linux platform used by OCI commands.
func DefaultPlatform() string {
	return ociresolver.DefaultPlatform()
}

// Resolve returns the digest-pinned metadata for an OCI reference without
// opening the local content or image metadata stores.
func Resolve(ctx context.Context, ref, platform string) (*ResolveResult, error) {
	return (ociresolver.Resolver{}).Resolve(ctx, ref, platform)
}

// Pull retrieves and indexes the content needed by an OCI image.
func (p *ImagePipeline) Pull(ctx context.Context, req PullRequest) (*PullResult, error) {
	return p.content.Pull(ctx, req)
}

// Build converts OCI content into a bootable managed image.
func (p *ImagePipeline) Build(ctx context.Context, req BuildRequest) (*image.ImageRecord, error) {
	return p.builder.Build(ctx, req)
}
