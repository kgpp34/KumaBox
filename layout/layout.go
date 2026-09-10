// Package layout owns the on-disk namespace under a node root.
//
// Every managed path is derived here, so no other package has to join paths
// under the root by hand and no shell script has to keep its own copy of the
// directory list (docs/ARCHITECTURE.md §5, docs/HOST.md §1).
package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Errors reported by this package.
var (
	ErrRootEmpty        = errors.New("root path is empty")
	ErrRootNotAbsolute  = errors.New("root path must be absolute")
	ErrRootNotDirectory = errors.New("root path exists and is not a directory")
	ErrRootNotWritable  = errors.New("root directory is not writable")
	ErrStatFailed       = errors.New("cannot inspect the root directory")
)

// dirPerm is the mode for every KumaBox-owned directory.
const dirPerm os.FileMode = 0o700

// Root is a resolved node root directory.
type Root struct {
	dir string
}

// New resolves dir into a Root.
//
// The path is made absolute and canonical so that containment checks compare
// real paths. dir may be a symlink: it is resolved rather than rejected, because
// operators legitimately point the root at another volume.
func New(dir string) (Root, error) {
	if dir == "" {
		return Root{}, ErrRootEmpty
	}
	if !filepath.IsAbs(dir) {
		return Root{}, fmt.Errorf("%w: %q", ErrRootNotAbsolute, dir)
	}
	canonical, err := canonicalize(filepath.Clean(dir))
	if err != nil {
		return Root{}, err
	}
	return Root{dir: canonical}, nil
}

// Dir returns the root directory.
func (r Root) Dir() string { return r.dir }

// MetadataDir holds the fact database.
func (r Root) MetadataDir() string { return filepath.Join(r.dir, "metadata") }

// DatabaseFile is the SQLite fact database.
func (r Root) DatabaseFile() string { return filepath.Join(r.MetadataDir(), "kumabox.db") }

// BlobsDir holds content-addressed objects: one file per digest.
func (r Root) BlobsDir() string { return filepath.Join(r.dir, "blobs") }

// TempDir is where a command stages work before publishing it atomically.
func (r Root) TempDir() string { return filepath.Join(r.dir, "tmp") }

// State describes what a root looks like right now.
type State struct {
	Exists     bool   `json:"exists"`
	Writable   bool   `json:"writable"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// Inspect reports the state of the root without changing it. When the root does
// not exist yet, free space is measured on the nearest existing ancestor,
// because that is the volume Prepare would write to.
func (r Root) Inspect() (State, error) {
	info, err := os.Stat(r.dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return State{}, fmt.Errorf("%w: %q", ErrRootNotDirectory, r.dir)
		}
		total, free, err := statfs(r.dir)
		if err != nil {
			return State{}, fmt.Errorf("%w: %q: %w", ErrStatFailed, r.dir, err)
		}
		return State{
			Exists:     true,
			Writable:   writable(r.dir),
			TotalBytes: total,
			FreeBytes:  free,
		}, nil
	case errors.Is(err, os.ErrNotExist):
		ancestor := nearestExisting(r.dir)
		total, free, err := statfs(ancestor)
		if err != nil {
			return State{}, fmt.Errorf("%w: %q: %w", ErrStatFailed, ancestor, err)
		}
		return State{Writable: writable(ancestor), TotalBytes: total, FreeBytes: free}, nil
	default:
		return State{}, fmt.Errorf("%w: %q: %w", ErrStatFailed, r.dir, err)
	}
}

// Prepare creates the managed directories and checks that they are writable. It
// is the only function here that modifies the filesystem, and it only ever
// touches KumaBox-owned paths.
func (r Root) Prepare() error {
	for _, dir := range []string{r.dir, r.MetadataDir(), r.BlobsDir(), r.TempDir()} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrRootNotWritable, dir, err)
		}
	}
	if !writable(r.dir) {
		return fmt.Errorf("%w: %q", ErrRootNotWritable, r.dir)
	}
	return nil
}

// canonicalize resolves symlinks along the longest existing prefix of path and
// re-appends the missing suffix, so the result is canonical without requiring
// the path to exist.
func canonicalize(path string) (string, error) {
	existing, suffix := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			if suffix == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %q: %w", ErrStatFailed, existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return path, nil
		}
		suffix = filepath.Join(filepath.Base(existing), suffix)
		existing = parent
	}
}

// nearestExisting returns the deepest existing ancestor of path.
func nearestExisting(path string) string {
	current := path
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return current
		}
		if _, err := os.Stat(parent); err == nil {
			return parent
		}
		current = parent
	}
}

// writable reports whether a new file can be created in dir. Creating and
// removing a probe file is the only reliable check across the permission models
// KumaBox runs under.
func writable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".kumabox-write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}
