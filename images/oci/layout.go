package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/images"
)

func NewLayout(path string) (images.Source, error) {
	return NewLayoutWithLimits(path, DefaultLimits())
}

func NewLayoutWithLimits(path string, limits Limits) (images.Source, error) {
	if !limits.Valid() {
		return nil, invalidSource("OCI size limits must be positive and bounded")
	}
	source := &resolvedSource{limits: limits}
	source.resolve = func(ctx context.Context, platform images.Platform) (v1.Image, error) {
		if err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return invalidSource("OCI layout contains symlink %s", path)
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return invalidSource("OCI layout contains special file %s", path)
			}
			if filepath.Base(path) == "index.json" || filepath.Base(path) == "oci-layout" {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Size() > maxMetadataSize {
					return invalidSource("OCI metadata exceeds size limit")
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		layoutRaw, err := readLocal(ctx, path, "oci-layout", maxMetadataSize)
		if err != nil {
			return nil, err
		}
		var layoutVersion struct {
			Version string `json:"imageLayoutVersion"`
		}
		if err := json.Unmarshal(layoutRaw, &layoutVersion); err != nil || layoutVersion.Version != "1.0.0" {
			return nil, invalidSource("invalid OCI layout version")
		}
		indexRaw, err := readLocal(ctx, path, "index.json", maxMetadataSize)
		if err != nil {
			return nil, err
		}
		index := &localIndex{path: path, raw: indexRaw, ctx: ctx}
		return imageForPlatform(index, platform)
	}
	return source, nil
}

func imageForPlatform(index v1.ImageIndex, platform images.Platform) (v1.Image, error) {
	candidates, err := platformCandidates(index, platform, 0)
	if err != nil {
		return nil, err
	}
	if len(candidates) != 1 {
		return nil, invalidSource("OCI layout has %d images for %s/%s; expected exactly one", len(candidates), platform.OS, platform.Architecture)
	}
	return candidates[0], nil
}

func platformCandidates(index v1.ImageIndex, platform images.Platform, depth int) ([]v1.Image, error) {
	if depth > 16 {
		return nil, invalidSource("OCI index nesting exceeds limit")
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, invalidSource("read OCI index: %v", err)
	}
	if manifest.SchemaVersion != 2 {
		return nil, invalidSource("unsupported OCI index schema version")
	}
	var candidates []v1.Image
	for _, descriptor := range manifest.Manifests {
		if err := validateDescriptor(descriptor, maxMetadataSize); err != nil {
			return nil, err
		}
		if descriptor.Platform != nil && (descriptor.Platform.OS != platform.OS || descriptor.Platform.Architecture != platform.Architecture) {
			continue
		}
		switch descriptor.MediaType {
		case types.OCIImageIndex, types.DockerManifestList:
			nested, err := index.ImageIndex(descriptor.Digest)
			if err != nil {
				return nil, err
			}
			raw, err := nested.RawManifest()
			if err != nil {
				return nil, err
			}
			if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
				return nil, err
			}
			images, err := platformCandidates(nested, platform, depth+1)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, images...)
		case types.OCIManifestSchema1, types.DockerManifestSchema2:
			image, err := index.Image(descriptor.Digest)
			if err != nil {
				return nil, err
			}
			raw, err := image.RawManifest()
			if err != nil {
				return nil, err
			}
			if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
				return nil, err
			}
			manifest, err := v1.ParseManifest(bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			if err := validateDescriptor(manifest.Config, maxMetadataSize); err != nil {
				return nil, err
			}
			configRaw, err := image.RawConfigFile()
			if err != nil {
				return nil, err
			}
			if err := checkBytes(configRaw, manifest.Config.Digest, manifest.Config.Size); err != nil {
				return nil, err
			}
			config, err := v1.ParseConfigFile(bytes.NewReader(configRaw))
			if err != nil {
				return nil, err
			}
			if config.OS == platform.OS && config.Architecture == platform.Architecture {
				candidates = append(candidates, image)
			}
		default:
			return nil, invalidSource("unsupported OCI descriptor media type %s", descriptor.MediaType)
		}
	}
	return candidates, nil
}
