// Package core owns application workflows that cross module boundaries and
// assembles their concrete adapters. Module-local policies remain with modules.
//
// Image command assembly:
//
//	storage.Roots --> images.Paths -----------+
//	                    |                    |
//	                    +--> SQLite --> catalog --> ImageStore
//	                                           |
//	                  source + EROFS + reporter +--> images.Importer
package core

import (
	"context"

	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/images/erofs"
	"github.com/kumabox/kumabox/images/source"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// ImageStore owns the resources assembled for one image command.
// Call Close after using its catalog and managed artifact paths.
type ImageStore struct {
	// Paths locates persistent artifacts, staging directories, and image locks.
	Paths images.Paths
	// Catalog exposes image metadata operations backed by the owned store.
	Catalog images.Catalog
	// store owns the database connection released by Close.
	store metadata.Store
}

// OpenImages ensures managed directories and opens the image metadata catalog.
// It does not probe conversion tools, so metadata queries do not require EROFS.
func OpenImages(ctx context.Context, roots storage.Roots) (*ImageStore, error) {
	paths, err := images.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, paths.MetadataDB(), metadataCollections(), sqlite.DefaultOptions())
	if err != nil {
		return nil, err
	}
	imageCatalog := catalog.New(store, catalog.WithImageUsage(sandboxcatalog.Usage{}))
	return &ImageStore{Paths: paths, Catalog: imageCatalog, store: store}, nil
}

// Close releases the metadata store after all catalog operations have finished.
func (s *ImageStore) Close() error { return s.store.Close() }

// NewImageImporter adds a converter only when an operation needs to import layers.
func NewImageImporter(ctx context.Context, store *ImageStore, reporter images.Reporter, platform types.Platform) (*images.Importer, error) {
	options := images.DefaultOptions()
	converter, err := erofs.New(ctx, platform.Architecture, options.Limits)
	if err != nil {
		return nil, err
	}
	return images.NewImporter(store.Paths, store.Catalog, converter, reporter, options)
}

// LocalImageOptions selects a local source without exposing adapter types to callers.
type LocalImageOptions struct {
	// Format is auto, docker, or oci; auto detects the source contents.
	Format string
	// SourceTag selects an image inside a multi-image Docker save archive.
	SourceTag string
}

// Validate rejects unsupported formats before a command opens its metadata store.
func (o LocalImageOptions) Validate() error {
	_, err := source.ParseFormat(o.Format)
	return err
}

// OpenLocalSource detects or selects the local adapter and stages archives as needed.
// On success, the caller must invoke the returned cleanup after using the source.
// Directory sources also return cleanup, allowing the caller to use one lifecycle.
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

// metadataCollections declares the complete schema opened by every command.
// Initializing all collections together prevents command order from changing
// the database shape without an explicit migration.
func metadataCollections() []metadata.Collection {
	result := catalog.Collections()
	return append(result, sandboxcatalog.Collections()...)
}
