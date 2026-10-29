package types

import (
	"errors"
	"path"
	"strings"
	"time"
)

// FileInfo describes one guest filesystem entry without following its final
// symlink. Mode contains permission bits; Kind describes the entry type.
type FileInfo struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
	LinkTarget string    `json:"link_target,omitempty"`
}

// FileWriteOptions controls atomic replacement in the guest. With no Mode,
// an existing regular file retains its permissions; a new file uses 0600.
type FileWriteOptions struct {
	Mode    *uint32 `json:"mode,omitempty"`
	Parents bool    `json:"parents,omitempty"`
}

// Validate checks permission bits before a transfer opens its transport.
func (o FileWriteOptions) Validate() error {
	if o.Mode != nil && *o.Mode > 0o777 {
		return errors.New("file mode must contain only permission bits (0000..0777)")
	}
	return nil
}

// ValidateGuestPath requires a canonical absolute POSIX path. These paths
// address the guest filesystem, including explicitly shared guest mounts.
// Destructive operations may not target its root directory.
func ValidateGuestPath(value string, destructive bool) error {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") || !path.IsAbs(value) || path.Clean(value) != value {
		return errors.New("guest path must be a clean absolute path without NUL or newlines (at most 4096 bytes)")
	}
	if destructive && value == "/" {
		return errors.New("cannot replace, rename or remove the guest root directory")
	}
	return nil
}
