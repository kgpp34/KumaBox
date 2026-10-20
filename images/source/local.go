package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	mediatypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/images"
)

// Format identifies local image metadata conventions, independently of tar compression.
type Format string

const (
	// FormatAuto detects OCI metadata first, then Docker save metadata.
	FormatAuto Format = "auto"
	// FormatOCI selects an OCI image layout, including one staged from an archive.
	FormatOCI Format = "oci"
	// FormatDocker selects Docker save metadata; Docker export archives are unsupported.
	FormatDocker Format = "docker"
)

// ParseFormat validates the CLI format vocabulary; an empty value means auto.
func ParseFormat(value string) (Format, error) {
	format := Format(value)
	switch format {
	case "", FormatAuto:
		return FormatAuto, nil
	case FormatOCI, FormatDocker:
		return format, nil
	default:
		return "", invalidSource("unsupported image format %q; use auto, oci, or docker", value)
	}
}

// LocalOptions controls local format selection and source resource budgets.
type LocalOptions struct {
	// Format selects metadata explicitly or requests detection with FormatAuto.
	Format Format
	// SourceTag selects one tagged Docker entry and is rejected for OCI layouts.
	SourceTag string
	// Limits bounds metadata-adjacent source content; zero uses images.DefaultLimits.
	Limits images.Limits
}

// OpenLocal owns format selection and archive staging. The caller must clean up
// after it finishes reading the source, including when Resolve or import fails.
// The returned cleanup function owns only staging created by this call; directory
// inputs remain caller-owned. Failure cleans staging before returning.
//
//	input --> directory? -- yes --> select metadata --> images.Source
//	             |                       ^
//	             no                      |
//	             +--> bounded staging ---+
//	                                      (cleanup after source consumption)
func OpenLocal(ctx context.Context, path, stagingRoot string, options LocalOptions) (images.Source, func() error, error) {
	format, err := ParseFormat(string(options.Format))
	if err != nil {
		return nil, nil, err
	}
	options.Format = format
	if options.Limits == (images.Limits{}) {
		options.Limits = images.DefaultLimits()
	}
	if !options.Limits.Valid() {
		return nil, nil, invalidSource("image size limits must be positive and bounded")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect image source: %w", err)
	}
	cleanup := func() error { return nil }
	if !info.IsDir() {
		path, cleanup, err = stageArchive(ctx, path, stagingRoot, options.Limits)
		if err != nil {
			return nil, nil, err
		}
	}
	source, err := openLocalFormat(ctx, path, options)
	if err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return source, cleanup, nil
}

// openLocalFormat dispatches one selected format without validation fallback.
func openLocalFormat(ctx context.Context, path string, options LocalOptions) (images.Source, error) {
	format := options.Format
	if format == FormatAuto {
		var err error
		format, err = detectLocalFormat(ctx, path)
		if err != nil {
			return nil, err
		}
	}
	switch format {
	case FormatOCI:
		if options.SourceTag != "" {
			return nil, invalidSource("--source-tag selects a Docker image; use --format docker")
		}
		return NewLayoutWithLimits(path, options.Limits)
	case FormatDocker:
		return newDockerSource(path, options)
	default:
		return nil, invalidSource("unsupported image format %q", format)
	}
}

// detectLocalFormat checks regular metadata markers in priority order.
// Modern Docker saves can contain both formats. Prefer OCI metadata and never
// fall back to another format after a recognized source fails validation.
func detectLocalFormat(ctx context.Context, path string) (Format, error) {
	for _, candidate := range []struct {
		// format is selected when its marker is present.
		format Format
		// marker is metadata that must be a regular file.
		marker string
	}{{FormatOCI, "oci-layout"}, {FormatDocker, "manifest.json"}} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info, err := os.Lstat(filepath.Join(path, candidate.marker))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("detect image format: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", invalidSource("image format marker %s is not a regular file", candidate.marker)
		}
		return candidate.format, nil
	}
	return "", invalidSource("unrecognized image format; expected an OCI layout/archive or a docker save archive")
}

// readLocal bounds metadata before allocation and closes the file and root.
// os.Root confines path resolution even if paths change concurrently; it does not
// make the contents inside the root immutable.
func readLocal(ctx context.Context, path, name string, limit int64) ([]byte, error) {
	reader, err := openLocal(ctx, path, name)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, invalidSource("image metadata exceeds %d bytes", limit)
	}
	return raw, nil
}

// openLocal opens a regular object through os.Root, preventing relative paths or
// symlink traversal from escaping the source root. Close owns both file and root.
func openLocal(ctx context.Context, path, name string) (io.ReadCloser, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = invalidSource("image source object is not a regular file")
		}
		return nil, errors.Join(err, file.Close(), root.Close())
	}
	return &localReader{Reader: &contextInput{ctx: ctx, source: file}, file: file, root: root}, nil
}

// localReader keeps the source root alive for the lifetime of an open object.
type localReader struct {
	// Reader checks cancellation before reading the file.
	io.Reader
	// file is the regular object opened within root.
	file *os.File
	// root anchors path resolution until Close.
	root *os.Root
}

// Close releases the object and its root while preserving both errors.
func (r *localReader) Close() error { return errors.Join(r.file.Close(), r.root.Close()) }

// fileLayer adapts an on-disk layer to the library's encoded-layer contract.
// Content verification is performed by resolvedSource when the stream is read.
type fileLayer struct {
	// path is the layout or staging root.
	path string
	// object is a relative layer path within path.
	object string
	// descriptor records encoded media type, digest, and size.
	descriptor v1.Descriptor
	// ctx belongs to the resolution that selected this layer.
	ctx context.Context
}

var _ partial.CompressedLayer = (*fileLayer)(nil)

// Digest returns the encoded content identity recorded in the descriptor.
func (l *fileLayer) Digest() (v1.Hash, error) { return l.descriptor.Digest, nil }

// Size returns the declared encoded byte count for later stream validation.
func (l *fileLayer) Size() (int64, error) { return l.descriptor.Size, nil }

// MediaType identifies the decoder required for the stored object.
func (l *fileLayer) MediaType() (mediatypes.MediaType, error) { return l.descriptor.MediaType, nil }

// Compressed opens encoded bytes inside the source root; the caller owns Close.
func (l *fileLayer) Compressed() (io.ReadCloser, error) {
	return openLocal(l.ctx, l.path, l.object)
}
