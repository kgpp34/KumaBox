package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	mediatypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

// NewLayout opens an OCI image layout using default source limits. Validation
// and platform selection occur during Resolve; the directory remains caller-owned.
func NewLayout(path string) (images.Source, error) {
	return NewLayoutWithLimits(path, images.DefaultLimits())
}

// NewLayoutWithLimits opens a caller-owned OCI directory with explicit budgets.
// Resolve rejects symlinks and special files, validates layout metadata, and
// requires one image matching the requested OS and architecture. Object reads use
// os.Root confinement in addition to the initial directory walk.
func NewLayoutWithLimits(path string, limits images.Limits) (images.Source, error) {
	if !limits.Valid() {
		return nil, invalidSource("OCI size limits must be positive and bounded")
	}
	source := &resolvedSource{limits: limits}
	source.resolve = func(ctx context.Context, platform types.Platform) (v1.Image, error) {
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
			// Version must match the supported OCI layout version 1.0.0.
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

// imageForPlatform rejects absent or ambiguous matches rather than choosing by order.
func imageForPlatform(index v1.ImageIndex, platform types.Platform) (v1.Image, error) {
	candidates, err := platformCandidates(index, platform, 0)
	if err != nil {
		return nil, err
	}
	if len(candidates) != 1 {
		return nil, invalidSource("OCI layout has %d images for %s/%s; expected exactly one", len(candidates), platform.OS, platform.Architecture)
	}
	return candidates[0], nil
}

// platformCandidates descends bounded OCI indices and verifies each traversed
// descriptor against object bytes. Platform hints filter branches; image configs
// determine the actual platform before a leaf becomes a candidate.
//
//	index --> descriptor validation --> platform hint matches?
//	                                         |
//	                       +-----------------+------------------+
//	                       |                                    |
//	                   nested index                         image manifest
//	                       |                                    |
//	                recurse (depth bound)             config platform check
//	                       +-----------------+------------------+
//	                                         |
//	                               exactly one candidate
func platformCandidates(index v1.ImageIndex, platform types.Platform, depth int) ([]v1.Image, error) {
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
		case mediatypes.OCIImageIndex, mediatypes.DockerManifestList:
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
		case mediatypes.OCIManifestSchema1, mediatypes.DockerManifestSchema2:
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

// blobName converts a supported digest into the confined OCI blob path.
func blobName(hash v1.Hash) (string, error) {
	digest, err := types.ParseDigest(hash.String())
	if err != nil {
		return "", invalidSource("invalid OCI blob digest: %v", err)
	}
	return "blobs/sha256/" + digest.Hex(), nil
}

// localIndex adapts bounded index bytes and lazily resolves child blob objects.
type localIndex struct {
	// path is the caller-owned layout or private staging root.
	path string
	// raw contains the already bounded index metadata.
	raw []byte
	// ctx propagates cancellation to child object reads.
	ctx context.Context
}

// MediaType identifies this adapter as an OCI image index.
func (i *localIndex) MediaType() (mediatypes.MediaType, error) { return mediatypes.OCIImageIndex, nil }

// Digest derives index identity from its exact serialized metadata bytes.
func (i *localIndex) Digest() (v1.Hash, error) { return partial.Digest(i) }

// Size reports the index byte count for descriptor validation.
func (i *localIndex) Size() (int64, error) { return int64(len(i.raw)), nil }

// RawManifest returns a copy so callers cannot mutate retained index bytes.
func (i *localIndex) RawManifest() ([]byte, error) { return bytes.Clone(i.raw), nil }

// IndexManifest parses the retained bytes for descriptor traversal.
func (i *localIndex) IndexManifest() (*v1.IndexManifest, error) {
	return v1.ParseIndexManifest(bytes.NewReader(i.raw))
}

// descriptor restricts child lookup to a bounded descriptor declared by this index.
func (i *localIndex) descriptor(hash v1.Hash) (v1.Descriptor, error) {
	manifest, err := i.IndexManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.Digest == hash {
			return descriptor, validateDescriptor(descriptor, maxMetadataSize)
		}
	}
	return v1.Descriptor{}, fmt.Errorf("OCI descriptor %s not found", hash)
}

// Image opens and verifies a child manifest before adapting its lazy layers.
func (i *localIndex) Image(hash v1.Hash) (v1.Image, error) {
	descriptor, err := i.descriptor(hash)
	if err != nil {
		return nil, err
	}
	name, err := blobName(hash)
	if err != nil {
		return nil, err
	}
	raw, err := readLocal(i.ctx, i.path, name, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&localImage{path: i.path, raw: raw, descriptor: descriptor, ctx: i.ctx})
}

// ImageIndex opens and verifies a nested index within the same root.
func (i *localIndex) ImageIndex(hash v1.Hash) (v1.ImageIndex, error) {
	descriptor, err := i.descriptor(hash)
	if err != nil {
		return nil, err
	}
	name, err := blobName(hash)
	if err != nil {
		return nil, err
	}
	raw, err := readLocal(i.ctx, i.path, name, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
		return nil, err
	}
	return &localIndex{path: i.path, raw: raw, ctx: i.ctx}, nil
}

// localImage keeps verified manifest bytes while config and layer files stay lazy.
type localImage struct {
	// path anchors all child object reads.
	path string
	// raw contains manifest bytes verified against descriptor.
	raw []byte
	// descriptor records the parent index's image identity.
	descriptor v1.Descriptor
	// ctx propagates cancellation to local file reads.
	ctx context.Context
}

// MediaType preserves the manifest media type declared by the parent index.
func (i *localImage) MediaType() (mediatypes.MediaType, error) { return i.descriptor.MediaType, nil }

// RawManifest returns a copy of the previously verified manifest bytes.
func (i *localImage) RawManifest() ([]byte, error) { return bytes.Clone(i.raw), nil }

// RawConfigFile bounds the declared config object; callers verify its digest.
func (i *localImage) RawConfigFile() ([]byte, error) {
	manifest, err := v1.ParseManifest(bytes.NewReader(i.raw))
	if err != nil {
		return nil, err
	}
	if err := validateDescriptor(manifest.Config, maxMetadataSize); err != nil {
		return nil, err
	}
	name, err := blobName(manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	return readLocal(i.ctx, i.path, name, maxMetadataSize)
}

// LayerByDigest adapts only layers declared by this manifest through fileLayer.
func (i *localImage) LayerByDigest(hash v1.Hash) (partial.CompressedLayer, error) {
	manifest, err := v1.ParseManifest(bytes.NewReader(i.raw))
	if err != nil {
		return nil, err
	}
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest == hash {
			object, err := blobName(hash)
			if err != nil {
				return nil, err
			}
			return &fileLayer{path: i.path, object: object, descriptor: descriptor, ctx: i.ctx}, nil
		}
	}
	return nil, fmt.Errorf("OCI layer %s not found", hash)
}
