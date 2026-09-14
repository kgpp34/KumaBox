// Package core assembles application modules and their concrete dependencies.
package core

import (
	"context"

	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/images/erofs"
	"github.com/kumabox/kumabox/images/source"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/storage"
)

// ImageStore owns the resources assembled for one image command.
// Call Close after using its catalog and managed artifact paths.
type ImageStore struct {
	Paths   images.Paths
	Catalog images.Catalog
	store   metadata.Store
}

func OpenImages(ctx context.Context, roots storage.Roots) (*ImageStore, error) {
	paths, err := images.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, paths.MetadataDB(), catalog.Collections(), sqlite.DefaultOptions())
	if err != nil {
		return nil, err
	}
	return &ImageStore{Paths: paths, Catalog: catalog.New(store), store: store}, nil
}

func (s *ImageStore) Close() error { return s.store.Close() }

// NewImageImporter adds a converter only when an operation needs to import layers.
func NewImageImporter(ctx context.Context, store *ImageStore, reporter images.Reporter, platform images.Platform) (*images.Importer, error) {
	options := images.DefaultOptions()
	converter, err := erofs.New(ctx, platform.Architecture, options.Limits)
	if err != nil {
		return nil, err
	}
	return images.NewImporter(store.Paths, store.Catalog, converter, reporter, options)
}

// LocalImageOptions selects a local source without exposing adapter types to callers.
type LocalImageOptions struct {
	Format    string
	SourceTag string
}

func (o LocalImageOptions) Validate() error {
	_, err := source.ParseFormat(o.Format)
	return err
}

// OpenLocalSource returns a source and the cleanup required for staged archives.
func (s *ImageStore) OpenLocalSource(ctx context.Context, path string, options LocalImageOptions) (images.Source, func() error, error) {
	format, err := source.ParseFormat(options.Format)
	if err != nil {
		return nil, nil, err
	}
	return source.OpenLocal(ctx, path, s.Paths.StagingDir(), source.LocalOptions{
		Format: format, SourceTag: options.SourceTag, Limits: images.DefaultLimits(),
	})
}

// NewRegistrySource selects the registry adapter and returns the normalized local name.
func NewRegistrySource(reference string) (images.Source, string, error) {
	return source.NewRegistry(reference)
}
