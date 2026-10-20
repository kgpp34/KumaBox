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
	"github.com/kumabox/kumabox/types"
)

// Paths derives managed image, metadata, staging and lock paths from validated roots.
// Source digests key shared EROFS and boot artifacts; hashes of converted files
// are stored separately in layer metadata.
type Paths struct {
	// roots is validated once so all derived paths share the same storage boundary.
	roots storage.Roots
}

// NewPaths validates roots without creating directories.
func NewPaths(roots storage.Roots) (Paths, error) {
	validated, err := roots.Validate()
	if err != nil {
		return Paths{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return Paths{roots: validated}, nil
}

// Ensure creates the managed artifact, staging and lock directories safely.
func (p Paths) Ensure() error {
	for _, path := range []string{p.LayersDir(), p.BootBaseDir(), p.StagingDir(), p.LocksDir()} {
		if err := storage.EnsureDir(path); err != nil {
			return err
		}
	}
	return nil
}

// LayersDir is the shared EROFS directory keyed by source SHA-256 digest.
func (p Paths) LayersDir() string { return filepath.Join(p.roots.Data, "images", "layers", "sha256") }

// BootBaseDir contains per-source-layer boot artifact directories.
func (p Paths) BootBaseDir() string { return filepath.Join(p.roots.Data, "images", "boot", "sha256") }

// StagingDir contains disposable work directories for local imports.
func (p Paths) StagingDir() string { return filepath.Join(p.roots.Data, "staging", "imports") }

// LocksDir contains runtime advisory locks shared by import, verify and removal.
func (p Paths) LocksDir() string { return filepath.Join(p.roots.Run, "locks", "images") }

// MetadataDB is the SQLite metadata path used by application assembly.
func (p Paths) MetadataDB() string { return filepath.Join(p.roots.Data, "meta", "meta.db") }

// EROFS returns the managed converted filesystem path for a source digest.
func (p Paths) EROFS(digest types.Digest) string {
	return filepath.Join(p.LayersDir(), digest.Hex()+".erofs")
}

// BootDir returns the extracted boot directory for a source digest.
func (p Paths) BootDir(digest types.Digest) string {
	return filepath.Join(p.BootBaseDir(), digest.Hex())
}

// BootFile validates a boot basename before joining it to the managed directory.
func (p Paths) BootFile(digest types.Digest, name string) (string, error) {
	if !IsBootName(name) {
		return "", fmt.Errorf("invalid boot artifact name %q", name)
	}
	return storage.Join(p.BootDir(digest), name)
}

// Kernel returns the conventional vmlinuz path; use BootFile for a selected versioned name.
func (p Paths) Kernel(digest types.Digest) string { return filepath.Join(p.BootDir(digest), "vmlinuz") }

// Initrd returns the conventional initrd.img path; use BootFile for a selected versioned name.
func (p Paths) Initrd(digest types.Digest) string {
	return filepath.Join(p.BootDir(digest), "initrd.img")
}

// Lock returns the advisory lock path protecting a source digest and its artifacts.
func (p Paths) Lock(digest types.Digest) string {
	return filepath.Join(p.LocksDir(), digest.Hex()+".lock")
}

// NewStaging creates a unique work directory; the caller must remove it after use.
func (p Paths) NewStaging(pattern string) (string, error) {
	if err := p.Ensure(); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(p.StagingDir(), pattern)
	if err != nil {
		return "", fmt.Errorf("create image staging directory: %w", err)
	}
	return dir, nil
}

// digestFileContext rejects unsafe paths and non-regular artifacts before hashing.
// Cancellation is checked between reads and all file handles are closed on return.
func digestFileContext(ctx context.Context, path string) (types.Digest, int64, error) {
	if err := storage.CheckPath(path); err != nil {
		return types.Digest{}, 0, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return types.Digest{}, 0, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	if !info.Mode().IsRegular() {
		return types.Digest{}, 0, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("artifact %s is not a regular file", path))
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return types.Digest{}, 0, err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return types.Digest{}, 0, errors.Join(fmt.Errorf("open %s: %w", path, err), root.Close())
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		return types.Digest{}, 0, fmt.Errorf("hash %s: %w", path, err)
	}
	digest, err := types.ParseDigest(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
	return digest, size, err
}

// contextReader checks cancellation between reads; it cannot interrupt a blocked
// underlying Read, so sources must also implement their own cancellation.
type contextReader struct {
	// ctx stops further reads after cancellation.
	ctx context.Context
	// reader supplies the artifact bytes without taking ownership of its lifetime.
	reader io.Reader
}

// Read forwards data only while the context remains active.
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
