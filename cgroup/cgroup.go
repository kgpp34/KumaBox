// Package cgroup owns the minimal cgroup v2 scope used to contain each VMM.
// CPU admission and placement policies remain outside this package until the
// capacity phase; start currently applies weight and a vCPU-sized hard quota.
package cgroup

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	// Root is the Linux unified cgroup hierarchy.
	Root = "/sys/fs/cgroup"
	// DefaultParent contains KumaBox VMM scopes.
	DefaultParent = "/sys/fs/cgroup/kumabox.slice"
)

// Manager prepares and reclaims per-sandbox scopes under one cgroup v2 parent.
type Manager struct {
	// parent is an absolute path below the unified hierarchy.
	parent string
}

// New validates a cgroup parent without touching the host hierarchy.
func New(parent string) (*Manager, error) {
	if parent == "" {
		parent = DefaultParent
	}
	clean := filepath.Clean(parent)
	relative, err := filepath.Rel(Root, clean)
	if err != nil || !filepath.IsAbs(clean) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("cgroup parent %q must be below %s", parent, Root)
	}
	return &Manager{parent: clean}, nil
}

// Parent returns the configured hierarchy path for diagnostics.
func (m *Manager) Parent() string {
	if m == nil {
		return ""
	}
	return m.parent
}
