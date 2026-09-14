package images

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/errdefs"

	"github.com/kumabox/kumabox/storage"
)

type Paths struct {
	roots storage.Roots
}

func NewPaths(roots storage.Roots) (Paths, error) {
	validated, err := roots.Validate()
	if err != nil {
		return Paths{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return Paths{roots: validated}, nil
}

func (p Paths) Ensure() error {
	for _, path := range []string{p.LayersDir(), p.BootBaseDir(), p.StagingDir(), p.LocksDir()} {
		if err := storage.EnsureDir(path); err != nil {
			return err
		}
	}
	return nil
}

func (p Paths) LayersDir() string { return filepath.Join(p.roots.Data, "images", "layers", "sha256") }

func (p Paths) BootBaseDir() string { return filepath.Join(p.roots.Data, "images", "boot", "sha256") }
func (p Paths) StagingDir() string  { return filepath.Join(p.roots.Data, "staging", "imports") }
func (p Paths) LocksDir() string    { return filepath.Join(p.roots.Run, "locks", "images") }
func (p Paths) MetadataDB() string  { return filepath.Join(p.roots.Data, "meta", "meta.db") }

func (p Paths) EROFS(digest Digest) string {
	return filepath.Join(p.LayersDir(), digest.Hex()+".erofs")
}

func (p Paths) BootDir(digest Digest) string {
	return filepath.Join(p.BootBaseDir(), digest.Hex())
}

func (p Paths) BootFile(digest Digest, name string) (string, error) {
	if !IsBootName(name) {
		return "", fmt.Errorf("invalid boot artifact name %q", name)
	}
	return storage.Join(p.BootDir(digest), name)
}

func IsBootName(name string) bool {
	return filepath.Base(name) == name && !strings.HasSuffix(name, ".old") &&
		(strings.HasPrefix(name, "vmlinuz") || strings.HasPrefix(name, "initrd.img"))
}

func (p Paths) Kernel(digest Digest) string { return filepath.Join(p.BootDir(digest), "vmlinuz") }
func (p Paths) Initrd(digest Digest) string { return filepath.Join(p.BootDir(digest), "initrd.img") }

func (p Paths) Lock(digest Digest) string { return filepath.Join(p.LocksDir(), digest.Hex()+".lock") }

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
