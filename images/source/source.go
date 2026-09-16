// Package source adapts registry images, OCI layouts, and Docker save archives to
// the images.Source contract. Metadata selection and validation happen during
// Resolve; layer content is checked while the importer consumes OpenLayer streams.
// It owns source decoding and staging, while images owns artifact publication.
package source

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	mediatypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

// maxMetadataSize bounds accepted manifest, config, and index object sizes.
const maxMetadataSize = 16 << 20

// resolvedLayer connects a stored layer descriptor to its expected unpacked hash.
type resolvedLayer struct {
	// layer supplies the encoded bytes through the source adapter.
	layer v1.Layer
	// diffID is the config hash of the decoded tar stream.
	diffID types.Digest
	// mediaType selects gzip, zstd, or raw decoding.
	mediaType mediatypes.MediaType
}

// resolvedSource shares metadata and streaming checks across all source formats.
type resolvedSource struct {
	// resolve selects the format-specific image.
	resolve func(context.Context, types.Platform) (v1.Image, error)
	// mu protects replacement and lookup of the resolved layer map.
	mu sync.RWMutex
	// layers is populated only after successful metadata validation.
	layers map[types.Digest]resolvedLayer
	// limits bounds encoded and unpacked layer content.
	limits images.Limits
}

var _ images.Source = (*resolvedSource)(nil)

// Resolve validates the selected manifest, config, platform, and layer descriptors
// before publishing the layer lookup used by OpenLayer. Encoded digest, size, and
// unpacked diffID are checked during layer consumption, even when an adapter has
// already inspected encoded objects while constructing the image.
func (s *resolvedSource) Resolve(ctx context.Context, platform types.Platform) (types.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return types.Manifest{}, err
	}
	image, err := s.resolve(ctx, platform)
	if err != nil {
		return types.Manifest{}, sourceError(err)
	}
	raw, err := image.RawManifest()
	if err != nil {
		return types.Manifest{}, sourceError(err)
	}
	if len(raw) > maxMetadataSize {
		return types.Manifest{}, invalidSource("manifest exceeds metadata limit")
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return types.Manifest{}, invalidSource("invalid OCI manifest: %v", err)
	}
	manifestHash, err := image.Digest()
	if err != nil {
		return types.Manifest{}, sourceError(err)
	}
	if err := checkBytes(raw, manifestHash, int64(len(raw))); err != nil {
		return types.Manifest{}, err
	}
	digest, err := types.ParseDigest(manifestHash.String())
	if err != nil {
		return types.Manifest{}, invalidSource("invalid manifest digest: %v", err)
	}
	if manifest.SchemaVersion != 2 {
		return types.Manifest{}, invalidSource("unsupported OCI manifest schema version")
	}
	if manifest.Config.MediaType != mediatypes.OCIConfigJSON && manifest.Config.MediaType != mediatypes.DockerConfigJSON {
		return types.Manifest{}, invalidSource("unsupported OCI config media type")
	}
	if err := validateDescriptor(manifest.Config, maxMetadataSize); err != nil {
		return types.Manifest{}, err
	}
	configRaw, err := image.RawConfigFile()
	if err != nil {
		return types.Manifest{}, sourceError(err)
	}
	if err := checkBytes(configRaw, manifest.Config.Digest, manifest.Config.Size); err != nil {
		return types.Manifest{}, err
	}
	config, err := v1.ParseConfigFile(bytes.NewReader(configRaw))
	if err != nil {
		return types.Manifest{}, invalidSource("invalid OCI config: %v", err)
	}
	if config.OS != platform.OS || config.Architecture != platform.Architecture {
		return types.Manifest{}, invalidSource("image platform %s/%s does not match %s/%s", config.OS, config.Architecture, platform.OS, platform.Architecture)
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return types.Manifest{}, invalidSource("config rootfs does not match manifest layers")
	}
	bootProfile := types.BootProfile(config.Config.Labels[types.ImageBootProfileLabel])
	layers := make(map[types.Digest]resolvedLayer)
	descriptors := make([]types.Descriptor, len(manifest.Layers))
	for position, desc := range manifest.Layers {
		if err := validateDescriptor(desc, s.limits.LayerSize); err != nil {
			return types.Manifest{}, err
		}
		switch desc.MediaType {
		case mediatypes.OCILayer, mediatypes.OCIUncompressedLayer, mediatypes.OCILayerZStd, mediatypes.DockerLayer, mediatypes.DockerUncompressedLayer:
		default:
			return types.Manifest{}, invalidSource("unsupported layer media type %s", desc.MediaType)
		}
		digest, err := types.ParseDigest(desc.Digest.String())
		if err != nil {
			return types.Manifest{}, invalidSource("invalid layer digest: %v", err)
		}
		diffID, err := types.ParseDigest(config.RootFS.DiffIDs[position].String())
		if err != nil {
			return types.Manifest{}, invalidSource("invalid layer diffID: %v", err)
		}
		layer, err := image.LayerByDigest(desc.Digest)
		if err != nil {
			return types.Manifest{}, sourceError(err)
		}
		if existing, ok := layers[digest]; ok && existing.diffID != diffID {
			return types.Manifest{}, invalidSource("repeated layer has inconsistent diffID")
		}
		layers[digest] = resolvedLayer{layer: layer, diffID: diffID, mediaType: desc.MediaType}
		descriptors[position] = types.Descriptor{Digest: digest, Size: desc.Size}
	}
	s.mu.Lock()
	s.layers = layers
	s.mu.Unlock()
	return types.Manifest{Digest: digest, Platform: platform, BootProfile: bootProfile, Layers: descriptors}, nil
}

// OpenLayer opens a previously resolved layer as a decoded tar stream. The caller
// must consume it to EOF to complete both hash checks, then close it on all paths.
// Closing an unread stream releases resources without validating the remainder.
//
//	encoded bytes --> size + digest check --> decoder --> limit + diffID check
//	                      ^                                |
//	                      +--- drain encoded remainder <---+ EOF
func (s *resolvedSource) OpenLayer(ctx context.Context, descriptor types.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	layer, ok := s.layers[descriptor.Digest]
	s.mu.RUnlock()
	if !ok {
		return nil, invalidSource("layer %s was not resolved", descriptor.Digest)
	}
	raw, err := layer.layer.Compressed()
	if err != nil {
		return nil, sourceError(err)
	}
	compressed := &checkedReader{ctx: ctx, reader: raw, hash: sha256.New(), expected: descriptor.Digest, limit: s.limits.LayerSize, size: descriptor.Size}
	buffered := bufio.NewReader(compressed)
	var input io.Reader = buffered
	closeDecoder := func() error { return nil }
	switch layer.mediaType {
	case mediatypes.OCILayer, mediatypes.DockerLayer:
		decoder, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, errors.Join(sourceError(err), raw.Close())
		}
		input, closeDecoder = decoder, decoder.Close
	case mediatypes.OCILayerZStd:
		decoder, err := zstd.NewReader(buffered, zstd.WithDecoderMaxMemory(uint64(max(1, s.limits.UnpackedSize))), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, errors.Join(sourceError(err), raw.Close())
		}
		input = decoder
		closeDecoder = func() error { decoder.Close(); return nil }
	}
	unpacked := &checkedReader{ctx: ctx, reader: input, hash: sha256.New(), expected: layer.diffID, limit: s.limits.UnpackedSize, size: -1}
	return &layerReader{unpacked: unpacked, buffered: buffered, raw: raw, closeDecoder: closeDecoder}, nil
}

// checkedReader enforces a stream budget and verifies its identity only at EOF.
// A terminal error is retained so retrying Read cannot bypass a failed check.
type checkedReader struct {
	// ctx is checked before each underlying read.
	ctx context.Context
	// reader supplies encoded bytes or the decoded tar stream.
	reader io.Reader
	// hash accumulates every byte returned by reader.
	hash hash.Hash
	// expected is the stored digest or unpacked diffID.
	expected types.Digest
	// limit is the maximum byte count; Read probes one extra byte for overflow.
	limit int64
	// size is the declared byte count, or -1 when no count is declared.
	size int64
	// read tracks bytes consumed for the size and budget checks.
	read int64
	// lastErr prevents reads after EOF, cancellation, or corruption.
	lastErr error
}

// Read detects limit violations immediately and digest or size mismatches at EOF.
func (r *checkedReader) Read(p []byte) (n int, returnErr error) {
	if r.lastErr != nil {
		return 0, r.lastErr
	}
	defer func() {
		if returnErr != nil {
			r.lastErr = returnErr
		}
	}()
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.limit-r.read+1 {
		p = p[:r.limit-r.read+1]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if _, hashErr := r.hash.Write(p[:n]); hashErr != nil {
		return n, hashErr
	}
	if r.read > r.limit {
		return n, invalidSource("layer exceeds %d bytes", r.limit)
	}
	if errors.Is(err, io.EOF) {
		if fmt.Sprintf("sha256:%x", r.hash.Sum(nil)) != r.expected.String() || (r.size >= 0 && r.size != r.read) {
			return n, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, fmt.Errorf("layer %s digest or size mismatch", r.expected))
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		err = sourceError(err)
	}
	return n, err
}

// layerReader couples decoder ownership with encoded and unpacked verification.
type layerReader struct {
	// unpacked verifies the decoded stream's diffID.
	unpacked *checkedReader
	// buffered retains encoded bytes read ahead by the decoder.
	buffered *bufio.Reader
	// raw owns the underlying file or registry response.
	raw io.ReadCloser
	// closeDecoder releases gzip or zstd state, if present.
	closeDecoder func() error
}

// Read drains encoded read-ahead at decoded EOF so its digest check also completes.
func (r *layerReader) Read(p []byte) (int, error) {
	n, err := r.unpacked.Read(p)
	if errors.Is(err, io.EOF) {
		if _, drainErr := io.Copy(io.Discard, r.buffered); drainErr != nil {
			return n, drainErr
		}
	}
	return n, err
}

// Close releases both decoder and input, preserving either cleanup failure.
func (r *layerReader) Close() error { return errors.Join(r.closeDecoder(), r.raw.Close()) }

// validateDescriptor accepts bounded SHA-256 objects and rejects external URLs.
func validateDescriptor(desc v1.Descriptor, limit int64) error {
	if _, err := types.ParseDigest(desc.Digest.String()); err != nil {
		return invalidSource("invalid descriptor: %v", err)
	}
	if desc.Size < 0 || desc.Size > limit {
		return invalidSource("descriptor size %d exceeds limit %d", desc.Size, limit)
	}
	if len(desc.URLs) != 0 {
		return invalidSource("external descriptor URLs are unsupported")
	}
	return nil
}

// checkBytes verifies a bounded metadata object against its declared identity.
func checkBytes(raw []byte, expected v1.Hash, size int64) error {
	if expected.Algorithm != "sha256" || fmt.Sprintf("%x", sha256.Sum256(raw)) != expected.Hex || int64(len(raw)) != size {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, fmt.Errorf("OCI object %s digest or size mismatch", expected))
	}
	return nil
}

// invalidSource classifies malformed or unsupported input as an argument error.
func invalidSource(format string, args ...any) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// sourceError preserves cancellation and classified errors, distinguishes gzip
// corruption, and treats other source I/O failures as unavailable artifacts.
func sourceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, err)
	}
	if _, ok := errdefs.CodeOf(err); ok {
		return err
	}
	return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
}
