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
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/images"
)

type Format string

const (
	FormatAuto   Format = "auto"
	FormatOCI    Format = "oci"
	FormatDocker Format = "docker"
)

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

type LocalOptions struct {
	Format    Format
	SourceTag string
	Limits    images.Limits
}

// OpenLocal owns format selection and archive staging. The caller must clean up
// after it finishes reading the source, including when Resolve or import fails.
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

// Modern Docker saves can contain both formats. Prefer OCI metadata and never
// fall back to another format after a recognized source fails validation.
func detectLocalFormat(ctx context.Context, path string) (Format, error) {
	for _, candidate := range []struct {
		format Format
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

// Local metadata is bounded before allocation; os.Root also contains concurrent path changes.
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

type localReader struct {
	io.Reader
	file *os.File
	root *os.Root
}

func (r *localReader) Close() error { return errors.Join(r.file.Close(), r.root.Close()) }

type fileLayer struct {
	path       string
	object     string
	descriptor v1.Descriptor
	ctx        context.Context
}

var _ partial.CompressedLayer = (*fileLayer)(nil)

func (l *fileLayer) Digest() (v1.Hash, error)            { return l.descriptor.Digest, nil }
func (l *fileLayer) Size() (int64, error)                { return l.descriptor.Size, nil }
func (l *fileLayer) MediaType() (types.MediaType, error) { return l.descriptor.MediaType, nil }
func (l *fileLayer) Compressed() (io.ReadCloser, error) {
	return openLocal(l.ctx, l.path, l.object)
}
