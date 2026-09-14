package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultDataRoot = "/var/lib/kumabox"
	DefaultRunRoot  = "/run/kumabox"
	DefaultLogRoot  = "/var/log/kumabox"
)

// Roots are the three host roots shared by KumaBox modules.
type Roots struct {
	Data string
	Run  string
	Log  string
}

func DefaultRoots() Roots {
	return Roots{Data: DefaultDataRoot, Run: DefaultRunRoot, Log: DefaultLogRoot}
}

// Validate normalizes roots and rejects ambiguous ownership boundaries.
func (r Roots) Validate() (Roots, error) {
	values := []*string{&r.Data, &r.Run, &r.Log}
	for _, value := range values {
		if *value == "" {
			return Roots{}, fmt.Errorf("storage root must not be empty")
		}
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return Roots{}, fmt.Errorf("resolve storage root %q: %w", *value, err)
		}
		*value = filepath.Clean(absolute)
		// macOS exposes these system directories through stable symlinks.
		for _, alias := range []string{"/tmp", "/var", "/etc"} {
			if within(*value, alias) {
				if resolved, err := filepath.EvalSymlinks(alias); err == nil {
					relative, err := filepath.Rel(alias, *value)
					if err != nil {
						return Roots{}, err
					}
					*value = filepath.Join(resolved, relative)
				}
			}
		}
		if err := CheckPath(*value); err != nil {
			return Roots{}, err
		}
	}
	paths := []string{r.Data, r.Run, r.Log}
	for i, left := range paths {
		for j, right := range paths {
			if i == j {
				continue
			}
			if within(left, right) {
				return Roots{}, fmt.Errorf("storage roots overlap: %s and %s", left, right)
			}
		}
	}
	return r, nil
}

// CheckPath refuses symlinks in every existing component, including the file.
func CheckPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect managed path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 && (current == "/tmp" || current == "/var" || current == "/etc") {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return err
			}
			current = resolved
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || (index < len(parts)-1 && !info.IsDir()) {
			return fmt.Errorf("managed path %s is not a real directory or file", current)
		}
	}
	return nil
}

// EnsureDir creates and durably publishes each missing directory.
func EnsureDir(path string) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("managed directory %s is not a real directory", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect directory %s: %w", path, err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("cannot create storage root %s", path)
	}
	if err := EnsureDir(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o750); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	if err := CheckPath(path); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("managed directory %s is not a real directory", path)
	}
	return syncPath(parent)
}

// Join returns a contained child path.
func Join(root string, elements ...string) (string, error) {
	for _, element := range elements {
		if element == "" || filepath.IsAbs(element) || element == ".." || strings.HasPrefix(element, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe managed path element %q", element)
		}
	}
	joined := filepath.Join(append([]string{root}, elements...)...)
	if !within(joined, root) {
		return "", fmt.Errorf("managed path %s escapes %s", joined, root)
	}
	if err := CheckPath(joined); err != nil {
		return "", err
	}
	return joined, nil
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
