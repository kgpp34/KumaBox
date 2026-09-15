package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	mediatypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

// dockerEntry is one image record in Docker save's manifest.json.
type dockerEntry struct {
	// Config names a config object by its content hash.
	Config string `json:"Config"`
	// RepoTags supplies optional source-tag selectors.
	RepoTags []string `json:"RepoTags"`
	// Layers records rootfs objects in base-to-top order.
	Layers []string `json:"Layers"`
}

// newDockerSource normalizes an optional tag and defers image selection to Resolve.
func newDockerSource(path string, options LocalOptions) (images.Source, error) {
	if options.SourceTag != "" {
		tag, err := name.NewTag(options.SourceTag)
		if err != nil {
			return nil, invalidSource("invalid Docker source tag %q", options.SourceTag)
		}
		options.SourceTag = tag.Name()
	}
	source := &resolvedSource{limits: options.Limits}
	source.resolve = func(ctx context.Context, platform types.Platform) (v1.Image, error) {
		entry, config, err := selectDockerEntry(ctx, path, platform, options.SourceTag)
		if err != nil {
			return nil, err
		}
		return dockerImageFromEntry(ctx, path, entry, config, options.Limits)
	}
	return source, nil
}

// selectDockerEntry requires exactly one tag/platform match. It verifies each
// relevant config identity before trusting platform or rootfs layer ordering.
func selectDockerEntry(ctx context.Context, path string, platform types.Platform, sourceTag string) (dockerEntry, []byte, error) {
	raw, err := readLocal(ctx, path, "manifest.json", maxMetadataSize)
	if err != nil {
		return dockerEntry{}, nil, err
	}
	var entries []dockerEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return dockerEntry{}, nil, invalidSource("invalid docker save manifest.json")
	}
	var selected dockerEntry
	var selectedConfig []byte
	var count int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return dockerEntry{}, nil, err
		}
		if sourceTag != "" && !dockerTagMatches(entry.RepoTags, sourceTag) {
			continue
		}
		config, err := readDockerConfig(ctx, path, entry.Config)
		if err != nil {
			return dockerEntry{}, nil, err
		}
		parsed, err := v1.ParseConfigFile(bytes.NewReader(config))
		if err != nil {
			return dockerEntry{}, nil, invalidSource("invalid Docker image config: %v", err)
		}
		if parsed.OS != platform.OS || parsed.Architecture != platform.Architecture {
			continue
		}
		if parsed.RootFS.Type != "layers" || len(parsed.RootFS.DiffIDs) != len(entry.Layers) {
			return dockerEntry{}, nil, invalidSource("Docker config rootfs does not match archive layers")
		}
		count++
		selected, selectedConfig = entry, config
	}
	if count == 0 {
		return dockerEntry{}, nil, invalidSource("no Docker image matches platform %s/%s and source tag %q", platform.OS, platform.Architecture, sourceTag)
	}
	if count != 1 {
		return dockerEntry{}, nil, invalidSource("Docker archive has %d matching images; select one with --source-tag", count)
	}
	return selected, selectedConfig, nil
}

// dockerTagMatches compares normalized tags, ignoring malformed archive tags.
func dockerTagMatches(tags []string, wanted string) bool {
	for _, value := range tags {
		tag, err := name.NewTag(value)
		if err == nil && tag.Name() == wanted {
			return true
		}
	}
	return false
}

// archiveObjectName rejects absolute paths and parent traversal in Docker metadata.
func archiveObjectName(value string) (string, error) {
	clean := filepath.Clean(value)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", invalidSource("unsafe image archive object path %q", value)
	}
	return clean, nil
}

// readDockerConfig verifies bounded config bytes against the hash in their filename.
func readDockerConfig(ctx context.Context, path, object string) ([]byte, error) {
	object, err := archiveObjectName(object)
	if err != nil {
		return nil, err
		// docker save names configs by their content hash (legacy .json names,
	}
	// sha256:hex names, or modern blobs/sha256/hex paths).
	hex := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(object), ".json"), "sha256:")
	digest, err := types.ParseDigest("sha256:" + hex)
	if err != nil {
		return nil, invalidSource("Docker config filename must contain its sha256 digest")
	}
	raw, err := readLocal(ctx, path, object, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, v1.Hash{Algorithm: "sha256", Hex: digest.Hex()}, int64(len(raw))); err != nil {
		return nil, err
	}
	return raw, nil
}

// dockerImageFromEntry normalizes the selected Docker entry into an OCI image.
// Docker archives do not retain a registry manifest. Build a deterministic
// manifest from the config and ordered layer descriptors, then reuse the same
// digest/diffID validation and streaming as every other resolvedSource.
// The synthetic digest need not equal the original registry manifest digest.
//
//	manifest.json --> tag + platform match --> verified config
//	                                              |
//	ordered layer files --> hash + media type -----+--> synthetic OCI manifest
//	                                                        |
//	                                              shared resolvedSource checks
func dockerImageFromEntry(ctx context.Context, path string, entry dockerEntry, config []byte, limits images.Limits) (v1.Image, error) {
	configHash := v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(config))}
	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     mediatypes.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: mediatypes.OCIConfigJSON, Digest: configHash, Size: int64(len(config))},
		Layers:        make([]v1.Descriptor, 0, len(entry.Layers)),
	}
	layers := make(map[v1.Hash]*fileLayer, len(entry.Layers))
	for _, object := range entry.Layers {
		object, err := archiveObjectName(object)
		if err != nil {
			return nil, err
		}
		descriptor, err := describeArchiveLayer(ctx, path, object, limits.LayerSize)
		if err != nil {
			return nil, err
		}
		manifest.Layers = append(manifest.Layers, descriptor)
		layers[descriptor.Digest] = &fileLayer{ctx: ctx, path: path, object: object, descriptor: descriptor}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&dockerImage{config: config, manifest: raw, layers: layers})
}

// describeArchiveLayer hashes bounded encoded bytes and detects compression by
// magic rather than filename. Modern content-addressed blob paths must match the
// computed hash; legacy layer paths obtain their identity from this hash pass.
func describeArchiveLayer(ctx context.Context, path, object string, limit int64) (v1.Descriptor, error) {
	reader, err := openLocal(ctx, path, object)
	if err != nil {
		return v1.Descriptor{}, err
	}
	buffered := bufio.NewReader(reader)
	magic, peekErr := buffered.Peek(4)
	media := mediatypes.OCIUncompressedLayer
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		media = mediatypes.OCILayer
	} else if len(magic) == 4 && bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		media = mediatypes.OCILayerZStd
	}
	if peekErr != nil && !errors.Is(peekErr, io.EOF) {
		return v1.Descriptor{}, errors.Join(peekErr, reader.Close())
	}
	digest, size, readErr := v1.SHA256(io.LimitReader(buffered, limit+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return v1.Descriptor{}, err
	}
	if size > limit {
		return v1.Descriptor{}, invalidSource("Docker layer exceeds %d bytes", limit)
	}
	if strings.HasPrefix(filepath.ToSlash(object), "blobs/sha256/") && filepath.Base(object) != digest.Hex {
		return v1.Descriptor{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, errors.New("docker layer blob digest mismatch"))
	}
	return v1.Descriptor{MediaType: media, Digest: digest, Size: size}, nil
}

// dockerImage supplies the normalized metadata and lazy file layer adapters.
type dockerImage struct {
	// config retains the verified Docker config unchanged.
	config []byte
	// manifest is the deterministic serialized OCI manifest.
	manifest []byte
	// layers indexes encoded objects by computed digest.
	layers map[v1.Hash]*fileLayer
}

var _ partial.CompressedImageCore = (*dockerImage)(nil)

// MediaType reports the synthetic OCI manifest format.
func (i *dockerImage) MediaType() (mediatypes.MediaType, error) {
	return mediatypes.OCIManifestSchema1, nil
}

// RawConfigFile returns an independent copy of the verified source config.
func (i *dockerImage) RawConfigFile() ([]byte, error) { return bytes.Clone(i.config), nil }

// RawManifest returns an independent copy of the synthetic manifest bytes.
func (i *dockerImage) RawManifest() ([]byte, error) { return bytes.Clone(i.manifest), nil }

// LayerByDigest rejects objects not included in the selected Docker image.
func (i *dockerImage) LayerByDigest(hash v1.Hash) (partial.CompressedLayer, error) {
	layer, ok := i.layers[hash]
	if !ok {
		return nil, invalidSource("Docker layer %s not found", hash)
	}
	return layer, nil
}
