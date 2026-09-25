// Package disk prepares and removes sandbox-owned persistent disks.
// It does not change lifecycle metadata or launch virtual machines.
package disk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const (
	// ext4MagicOffset is the superblock offset plus the s_magic field offset.
	ext4MagicOffset int64 = 1024 + 56
	// ext4Magic identifies a formatted ext2/3/4 filesystem superblock.
	ext4Magic uint16 = 0xef53
)

// Backend is the storage capability required by sandbox lifecycle workflows.
// Implementations own disk creation, integrity checks, and idempotent cleanup;
// they never mutate sandbox metadata.
type Backend interface {
	Prepare(context.Context, types.SandboxID, int64) error
	Check(context.Context, types.SandboxID, int64) error
	Remove(context.Context, types.SandboxID) error
}

// Ext4 prepares one sparse, private COW disk directly at its sandbox-owned path.
type Ext4 struct {
	// paths derives the final path from a validated sandbox ID.
	paths sandbox.Paths
	// mkfs is the executable name or test path invoked without a shell.
	mkfs string
}

var _ Backend = (*Ext4)(nil)

// NewExt4 creates a disk preparer using the configured mkfs.ext4 executable.
func NewExt4(paths sandbox.Paths, binary string) (*Ext4, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("ext4 formatter binary is required")
	}
	return &Ext4{paths: paths, mkfs: binary}, nil
}

// Prepare creates and formats the final COW path. The preceding Creating record
// owns any partial file, so this private resource needs no staging publication.
//
//	ensure sandbox dir -> O_EXCL sparse truncate -> mkfs.ext4 -> superblock check
func (d *Ext4) Prepare(ctx context.Context, id types.SandboxID, size int64) error {
	if d == nil || d.mkfs == "" {
		return errors.New("ext4 disk preparer is not configured")
	}
	if size < types.MinSandboxStorage {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("COW size must be at least %d bytes", types.MinSandboxStorage))
	}
	dir, err := d.paths.Dir(id)
	if err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	path, err := d.paths.COW(id)
	if err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := storage.EnsureDir(dir); err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err), "prepare sandbox disk", id.String(), "directory", "check data root permissions", false)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err), "prepare sandbox disk", id.String(), "open directory", "inspect the sandbox data directory", false)
	}
	file, err := root.OpenFile("cow.raw", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.Join(err, root.Close())), "prepare sandbox disk", id.String(), "create sparse file", "inspect the sandbox data directory", false)
	}
	truncateErr := file.Truncate(size)
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(truncateErr, closeErr); err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err), "prepare sandbox disk", id.String(), "create sparse file", "remove the failed sandbox", false)
	}
	if _, err := exec.LookPath(d.mkfs); err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err), "prepare sandbox disk", id.String(), "format ext4", "install e2fsprogs or run kumabox doctor --fix", false)
	}
	output, err := exec.CommandContext( //nolint:gosec // executable is fixed by production construction; path is derived from validated roots and UUID
		ctx, d.mkfs, "-F", "-m", "0", "-q", "-E", "lazy_itable_init=1,lazy_journal_init=1,discard", path,
	).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		return errdefs.Context(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err), "prepare sandbox disk", id.String(), "format ext4", "remove the failed sandbox after checking mkfs.ext4", false)
	}
	if err := validate(path, size); err != nil {
		return errdefs.Context(errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err), "prepare sandbox disk", id.String(), "validate ext4", "remove and recreate the sandbox", false)
	}
	return nil
}

// Check verifies that an existing sandbox COW is the expected regular ext4
// file. It never repairs or reformats data during a lifecycle operation.
func (d *Ext4) Check(_ context.Context, id types.SandboxID, size int64) error {
	if d == nil {
		return errors.New("ext4 disk store is not configured")
	}
	path, err := d.paths.COW(id)
	if err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := validate(path, size); err != nil {
		return errdefs.Context(
			errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err),
			"check sandbox disk", id.String(), "validate ext4", "remove and recreate the sandbox", false,
		)
	}
	return nil
}

// Remove deletes only the directory derived from a validated sandbox ID.
// Missing directories are already clean.
func (d *Ext4) Remove(_ context.Context, id types.SandboxID) error {
	dir, err := d.paths.Dir(id)
	if err != nil {
		return err
	}
	if err := storage.CheckPath(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove sandbox disk directory %s: %w", dir, err)
	}
	return nil
}

// validate checks the logical size and ext4 superblock without invoking another tool.
func validate(path string, expectedSize int64) error {
	if err := storage.CheckPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return fmt.Errorf("COW %s is not a regular %d-byte file", path, expectedSize)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return errors.Join(err, root.Close())
	}
	var magic [2]byte
	_, readErr := file.ReadAt(magic[:], ext4MagicOffset)
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(readErr, closeErr); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if binary.LittleEndian.Uint16(magic[:]) != ext4Magic {
		return fmt.Errorf("COW %s has no ext4 superblock", path)
	}
	return nil
}
