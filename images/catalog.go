package images

import (
	"context"
	"errors"
	"time"
)

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

type CatalogReader interface {
	Resolve(context.Context, string) (Image, error)
	List(context.Context) ([]Image, error)
	FindLayers(context.Context, []Digest) (map[Digest]Layer, error)
}

type CatalogWriter interface {
	CommitImport(context.Context, ImportCommit) error
	Remove(context.Context, string, Digest) (Removal, error)
}

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
