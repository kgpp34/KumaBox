// Package core owns application workflows that cross module boundaries and
// assembles their concrete adapters. Module-local policies remain with modules.
//
// Image command assembly:
//
//	config.Paths --> images.Paths ------------+
//	                    |                    |
//	                    +--> SQLite --> catalog --> ImageStore
//	                                           |
//	                  source + EROFS + reporter +--> images.Importer
package core

import (
	"context"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/images/erofs"
	"github.com/kumabox/kumabox/images/source"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
	networkcni "github.com/kumabox/kumabox/network/cni"
	sandboxcatalog "github.com/kumabox/kumabox/sandbox/catalog"
	"github.com/kumabox/kumabox/types"
)

// ImageStore owns the resources assembled for one image command.
// Call Close after using its catalog and managed artifact paths.
type ImageStore struct {
	// Paths locates persistent artifacts, staging directories, and image locks.
	Paths images.Paths
	// Catalog exposes image metadata operations backed by the owned store.
	Catalog images.Catalog
	// options is the validated image policy used by lazily created adapters.
	options config.Images
	// store owns the database connection released by Close.
	store metadata.Store
}

// OpenImages ensures managed directories and opens the image metadata catalog.
// It does not probe conversion tools, so metadata queries do not require EROFS.
func OpenImages(ctx context.Context, configuration config.Config) (*ImageStore, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	paths, err := images.NewPaths(configuration.Paths)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, paths.MetadataDB(), metadataCollections(), sqlite.Options{
		BusyTimeout: configuration.Metadata.BusyTimeout,
		RetryLimit:  configuration.Metadata.RetryLimit,
	})
	if err != nil {
		return nil, err
	}
	imageCatalog := catalog.New(store, catalog.WithImageUsage(sandboxcatalog.Usage{}))
	return &ImageStore{Paths: paths, Catalog: imageCatalog, options: configuration.Images, store: store}, nil
}

// Close releases the metadata store after all catalog operations have finished.
func (s *ImageStore) Close() error { return s.store.Close() }

// NewImageImporter adds a converter only when an operation needs to import layers.
func NewImageImporter(ctx context.Context, store *ImageStore, reporter images.Reporter, platform types.Platform) (*images.Importer, error) {
	options := images.Options{Limits: imageLimits(store.options), Parallelism: store.options.Parallelism, Now: time.Now}
	converter, err := erofs.New(ctx, platform.Architecture, erofs.Options{
		Binary: store.options.EROFSBinary,
		Limits: options.Limits,
	})
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
		Format: format, SourceTag: options.SourceTag, Limits: imageLimits(s.options),
	})
}

// imageLimits translates application configuration into the image module's
// immutable stream and artifact bounds.
func imageLimits(options config.Images) images.Limits {
	return images.Limits{
		LayerSize: options.LayerSize, UnpackedSize: options.UnpackedSize,
		BootSize: options.BootSize, ArchiveSize: options.ArchiveSize,
	}
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
	result = append(result, sandboxcatalog.Collections()...)
	return append(result, networkcni.Collections()...)
}
