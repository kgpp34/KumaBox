package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/storage"
)

const maxArchiveEntries = 1 << 20

func NewArchive(path, stagingRoot string) (images.Source, func() error, error) {
	return NewArchiveContext(context.TODO(), path, stagingRoot, DefaultLimits())
}

func NewArchiveContext(ctx context.Context, path, stagingRoot string, limits Limits) (images.Source, func() error, error) {
	if !limits.Valid() {
		return nil, nil, invalidSource("OCI size limits must be positive and bounded")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := storage.EnsureDir(stagingRoot); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(stagingRoot, "oci-archive-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create OCI archive staging: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := extractArchiveContext(ctx, path, dir, limits.ArchiveSize); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	source, err := NewLayoutWithLimits(dir, limits)
	if err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return source, cleanup, nil
}

func extractArchive(path, destination string) error {
	return extractArchiveContext(context.TODO(), path, destination, DefaultLimits().ArchiveSize)
}

func extractArchiveContext(ctx context.Context, path, destination string, limit int64) (returnErr error) {
	if limit <= 0 {
		return invalidSource("archive size limit must be positive")
	}
	file, err := openLocal(ctx, filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return sourceError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	buffered := bufio.NewReader(&contextInput{ctx: ctx, source: file})
	var source io.Reader = buffered
	magic, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		decoder, err := gzip.NewReader(buffered)
		if err != nil {
			return invalidSource("open compressed OCI archive: %v", err)
		}
		defer func() { returnErr = errors.Join(returnErr, decoder.Close()) }()
		source = decoder
	}
	bounded := &io.LimitedReader{R: &contextInput{ctx: ctx, source: source}, N: limit + 1}
	reader := tar.NewReader(bounded)
	var total int64
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= maxArchiveEntries {
			return invalidSource("OCI archive entry count exceeds limit")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, bounded); err != nil {
				return err
			}
			if bounded.N == 0 {
				return invalidSource("unpacked OCI archive exceeds %d bytes", limit)
			}
			return nil
		}
		if err != nil {
			return invalidSource("read OCI archive: %v", err)
		}
		clean := filepath.Clean(header.Name)
		if clean == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return invalidSource("unsafe OCI archive path %q", header.Name)
		}
		target, err := storage.Join(destination, clean)
		if err != nil {
			return err
		}
		if header.Size < 0 || header.Size > limit-total {
			return invalidSource("OCI archive exceeds size limit")
		}
		total += header.Size
		switch header.Typeflag {
		case tar.TypeDir:
			if err := storage.EnsureDir(target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := storage.EnsureDir(filepath.Dir(target)); err != nil {
				return err
			}
			root, err := os.OpenRoot(destination)
			if err != nil {
				return err
			}
			output, err := root.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return errors.Join(err, root.Close())
			}
			_, copyErr := io.CopyN(output, reader, header.Size)
			if err := errors.Join(copyErr, output.Close(), root.Close()); err != nil {
				return fmt.Errorf("extract OCI archive file: %w", err)
			}
		default:
			return invalidSource("unsupported OCI archive entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

type contextInput struct {
	ctx    context.Context
	source io.Reader
}

func (r *contextInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
