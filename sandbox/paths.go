// Package sandbox defines filesystem ownership and paths for sandbox resources.
// Shared sandbox data contracts live in types; application workflows live in core.
package sandbox

import (
	"path/filepath"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// Paths derives persistent sandbox disks and stable operation locks from shared roots.
type Paths struct {
	// roots was validated at construction so every derived path shares one boundary.
	roots storage.Roots
}

// NewPaths validates roots without creating any directories.
func NewPaths(roots storage.Roots) (Paths, error) {
	validated, err := roots.Validate()
	if err != nil {
		return Paths{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return Paths{roots: validated}, nil
}

// Ensure creates the persistent sandbox base and stable lock directory.
func (p Paths) Ensure() error {
	for _, path := range []string{p.DataDir(), p.LocksDir()} {
		if err := storage.EnsureDir(path); err != nil {
			return err
		}
	}
	return nil
}

// DataDir contains one persistent directory per sandbox ID.
func (p Paths) DataDir() string { return filepath.Join(p.roots.Data, "sandboxes") }

// LocksDir contains persistent-inode advisory locks for sandbox operations.
func (p Paths) LocksDir() string { return filepath.Join(p.roots.Run, "locks", "sandboxes") }

// Dir returns a sandbox's persistent directory after validating its ID.
func (p Paths) Dir(id types.SandboxID) (string, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return "", err
	}
	return storage.Join(p.DataDir(), id.String())
}

// COW returns the sandbox's private sparse ext4 disk path.
func (p Paths) COW(id types.SandboxID) (string, error) {
	dir, err := p.Dir(id)
	if err != nil {
		return "", err
	}
	return storage.Join(dir, "cow.raw")
}

// Lock returns the stable operation lock path for an ID.
func (p Paths) Lock(id types.SandboxID) (string, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return "", err
	}
	return storage.Join(p.LocksDir(), id.String()+".lock")
}
