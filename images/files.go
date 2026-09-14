package images

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
)

func digestFileContext(ctx context.Context, path string) (Digest, int64, error) {
	if err := storage.CheckPath(path); err != nil {
		return Digest{}, 0, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Digest{}, 0, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	if !info.Mode().IsRegular() {
		return Digest{}, 0, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("artifact %s is not a regular file", path))
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return Digest{}, 0, err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return Digest{}, 0, errors.Join(fmt.Errorf("open %s: %w", path, err), root.Close())
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		return Digest{}, 0, fmt.Errorf("hash %s: %w", path, err)
	}
	digest, err := ParseDigest(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
	return digest, size, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
