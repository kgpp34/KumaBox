package images

import (
	"context"
	"errors"
	"time"

	"github.com/kumabox/kumabox/types"
)

// ImportCommit contains image facts that a catalog must persist atomically.
// Artifact files must already have been published and verified by the caller.
type ImportCommit struct {
	// Name is the local alias to create or bind to the same existing manifest.
	Name string
	// Manifest identifies the image and defines the exact layer order.
	Manifest types.Manifest
	// Layers contains converted metadata in the same order as Manifest.Layers.
	Layers []types.Layer
	// Boot must equal the overlay-aware selection derived from Layers.
	Boot types.Boot
	// Size must equal the sum of converted layer sizes, without overflow.
	Size int64
	// Created supplies a nonzero timestamp for a newly registered manifest.
	Created time.Time
}

// Removal describes metadata already removed and artifacts eligible for deletion.
type Removal struct {
	// Names contains aliases deleted by the catalog transaction.
	Names []string
	// Layers contains source digests no longer referenced by any registered image.
	Layers []types.Digest
}

// CatalogReader reconstructs image facts from a consistent metadata snapshot.
type CatalogReader interface {
	// Resolve accepts an exact alias or an unambiguous manifest digest prefix.
	Resolve(context.Context, string) (types.Image, error)
	// List returns committed images with their aliases and ordered layers.
	List(context.Context) ([]types.Image, error)
	// FindLayers returns committed mappings and rejects conflicting shared artifacts.
	FindLayers(context.Context, []types.Digest) (map[types.Digest]types.Layer, error)
}

// CatalogWriter changes aliases, image facts and layer references atomically.
type CatalogWriter interface {
	// CommitImport registers validated facts; an alias bound elsewhere is a conflict.
	CommitImport(context.Context, ImportCommit) error
	// Remove deletes an alias, or all aliases for a digest reference, and returns
	// unreferenced layers. expected must still match the resolved manifest.
	Remove(context.Context, string, types.Digest) (Removal, error)
}

// Catalog combines the metadata contracts used by image management commands.
type Catalog interface {
	CatalogReader
	CatalogWriter
}

// Validate checks the image facts that must be committed together.
func (commit ImportCommit) Validate() error {
	if commit.Name == "" || commit.Manifest.Digest.IsZero() || !commit.Manifest.Platform.Valid() || len(commit.Layers) == 0 || len(commit.Layers) != len(commit.Manifest.Layers) || commit.Created.IsZero() {
		return errors.New("invalid image name, manifest, platform, layers or creation time")
	}
	var size int64
	for pos, layer := range commit.Layers {
		if layer.SourceDigest.IsZero() || layer.EROFSDigest.IsZero() || layer.SourceDigest != commit.Manifest.Layers[pos].Digest || layer.Size <= 0 || size > (1<<63-1)-layer.Size {
			return errors.New("invalid layer identity, order or size")
		}
		size += layer.Size
		seen := make(map[string]bool)
		for _, file := range layer.BootFiles {
			if !IsBootName(file.Name) || file.Digest.IsZero() || file.Size <= 0 || seen[file.Name] {
				return errors.New("invalid boot file metadata")
			}
			seen[file.Name] = true
		}
		for _, name := range layer.Whiteouts {
			if !IsBootName(name) {
				return errors.New("invalid boot whiteout")
			}
		}
	}
	boot, err := SelectBoot(commit.Layers)
	if err != nil || boot != commit.Boot || size != commit.Size {
		return errors.New("inconsistent boot selection or total image size")
	}
	return nil
}
