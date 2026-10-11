package source

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

// maxArchiveEntries bounds work from archives containing many tiny entries.
const maxArchiveEntries = 1 << 20

// NewArchive stages a tar or gzip-compressed OCI layout using default limits.
// Use OpenLocal for format detection or Docker save archives. The caller must run
// the returned cleanup after consuming the source; use NewArchiveContext to cancel.
func NewArchive(path, stagingRoot string) (images.Source, func() error, error) {
	return NewArchiveContext(context.TODO(), path, stagingRoot, images.DefaultLimits())
}

// NewArchiveContext stages a bounded OCI archive and returns source ownership
// separately from the cleanup function. Construction failure removes staging.
func NewArchiveContext(ctx context.Context, path, stagingRoot string, limits images.Limits) (images.Source, func() error, error) {
	dir, cleanup, err := stageArchive(ctx, path, stagingRoot, limits)
	if err != nil {
		return nil, nil, err
	}
	source, err := NewLayoutWithLimits(dir, limits)
	if err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return source, cleanup, nil
}

// stageArchive extracts into a private temporary directory under stagingRoot.
// Cleanup is returned only on success; partial extraction is removed on failure.
func stageArchive(ctx context.Context, path, stagingRoot string, limits images.Limits) (string, func() error, error) {
	if !limits.Valid() {
		return "", nil, invalidSource("archive size limits must be positive and bounded")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := storage.EnsureDir(stagingRoot); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(stagingRoot, "image-archive-*")
	if err != nil {
		return "", nil, fmt.Errorf("create image archive staging: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := extractArchiveContext(ctx, path, dir, limits.ArchiveSize); err != nil {
		return "", nil, errors.Join(err, cleanup())
	}
	return dir, cleanup, nil
}

// extractArchive is the default-budget, non-cancelable extraction helper.
func extractArchive(path, destination string) error {
	return extractArchiveContext(context.TODO(), path, destination, images.DefaultLimits().ArchiveSize)
}

// extractArchiveContext accepts plain tar or gzip by magic bytes, allowing only
// directories and newly created regular files. Path validation and os.Root keep
// file writes within destination; O_EXCL rejects duplicate file destinations.
// Both declared payload bytes and the full decoded tar stream are bounded.
//
//	file --> magic detection --> optional gzip --> decoded byte budget --> tar
//	                                                                   |
//	                 entry count + path + type + size checks <----------+
//	                                   |
//	                         exclusive regular file writes
//
// The EOF drain includes trailing gzip data in the byte budget and checksum.
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
			return invalidSource("open compressed image archive: %v", err)
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
			return invalidSource("image archive entry count exceeds limit")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, bounded); err != nil {
				return err
			}
			if bounded.N == 0 {
				return invalidSource("unpacked image archive exceeds %d bytes", limit)
			}
			return nil
		}
		if err != nil {
			return invalidSource("read image archive: %v", err)
		}
		clean := filepath.Clean(header.Name)
		if clean == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return invalidSource("unsafe image archive path %q", header.Name)
		}
		target, err := storage.Join(destination, clean)
		if err != nil {
			return err
		}
		if header.Size < 0 || header.Size > limit-total {
			return invalidSource("image archive exceeds size limit")
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
				return fmt.Errorf("extract image archive file: %w", err)
			}
		default:
			return invalidSource("unsupported image archive entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

// contextInput stops subsequent reads after cancellation; it cannot interrupt an
// underlying Read already in progress, which must support cancellation itself.
type contextInput struct {
	// ctx is checked immediately before each read.
	ctx context.Context
	// source supplies archive, file, or decoded bytes.
	source io.Reader
}

// Read checks cancellation before delegating to the underlying stream.
func (r *contextInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
