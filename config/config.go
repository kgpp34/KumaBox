// Package config resolves KumaBox settings.
//
// It is the only package that reads the environment (docs/ARCHITECTURE.md
// §12.1). Precedence is flag, then environment, then default. A configuration
// file joins this package once there is more than one setting worth putting in
// it.
package config

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	// EnvRoot overrides the node root directory.
	EnvRoot = "KUMABOX_ROOT"
	// DefaultRoot is the system-wide node root used by a package install.
	DefaultRoot = "/var/lib/kumabox"
)

// ErrRootNotAbsolute reports a root that is not an absolute path.
var ErrRootNotAbsolute = errors.New("root must be an absolute path")

// Config holds the resolved settings for one invocation.
type Config struct {
	// Root is the absolute node root directory.
	Root string
}

// Load resolves the configuration. flagRoot is the value of --root, or an
// empty string when the flag was not given.
func Load(flagRoot string) (Config, error) {
	root := flagRoot
	if root == "" {
		root = os.Getenv(EnvRoot)
	}
	if root == "" {
		root = DefaultRoot
	}
	if !filepath.IsAbs(root) {
		return Config{}, ErrRootNotAbsolute
	}
	return Config{Root: filepath.Clean(root)}, nil
}
