// Package storage prepares and validates durable VM-owned block devices.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultQEMUImgTimeout = 30 * time.Second

// OverlaySpec describes one qcow2 writable layer and its immutable backing file.
type OverlaySpec struct {
	Path       string
	BasePath   string
	BaseFormat string
}

// ImageInfo is the qemu-img metadata needed to validate an existing overlay.
type ImageInfo struct {
	Format          string `json:"format"`
	BackingFilename string `json:"backing-filename"`
	VirtualSize     int64  `json:"virtual-size"`
}

// QEMUImg is a bounded adapter around qemu-img. It never invokes a shell.
type QEMUImg struct {
	binary  string
	timeout time.Duration
}

// NewQEMUImg creates an adapter for binary.
func NewQEMUImg(binary string) *QEMUImg {
	return &QEMUImg{binary: binary, timeout: defaultQEMUImgTimeout}
}

// EnsureOverlay atomically creates an overlay or validates the existing file.
func (q *QEMUImg) EnsureOverlay(ctx context.Context, spec OverlaySpec) error {
	if err := validateOverlaySpec(spec); err != nil {
		return err
	}
	if _, err := os.Stat(spec.BasePath); err != nil {
		return fmt.Errorf("stat overlay base: %w", err)
	}
	if _, err := os.Stat(spec.Path); err == nil {
		return q.validateOverlay(ctx, spec)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat overlay: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o700); err != nil {
		return fmt.Errorf("create overlay owner directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(spec.Path), ".root-overlay-*.qcow2")
	if err != nil {
		return fmt.Errorf("create overlay staging file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close overlay staging file: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("prepare overlay staging path: %w", err)
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	if _, err := q.run(ctx, "create", "-f", "qcow2", "-F", spec.BaseFormat, "-b", spec.BasePath, tmpPath); err != nil {
		return fmt.Errorf("create qcow2 overlay: %w", err)
	}
	if err := q.validateOverlay(ctx, OverlaySpec{Path: tmpPath, BasePath: spec.BasePath, BaseFormat: spec.BaseFormat}); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("set overlay permissions: %w", err)
	}
	if err := os.Rename(tmpPath, spec.Path); err != nil {
		return fmt.Errorf("publish qcow2 overlay: %w", err)
	}
	return nil
}

// Info returns qemu-img metadata for path.
func (q *QEMUImg) Info(ctx context.Context, path string) (ImageInfo, error) {
	out, err := q.run(ctx, "info", "--output=json", path)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("inspect image: %w", err)
	}
	var info ImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return ImageInfo{}, fmt.Errorf("decode qemu-img info: %w", err)
	}
	return info, nil
}

func (q *QEMUImg) validateOverlay(ctx context.Context, spec OverlaySpec) error {
	info, err := q.Info(ctx, spec.Path)
	if err != nil {
		return err
	}
	if info.Format != "qcow2" {
		return fmt.Errorf("overlay format is %q, want qcow2", info.Format)
	}
	actualBase, err := filepath.Abs(info.BackingFilename)
	if err != nil {
		return fmt.Errorf("resolve overlay backing path: %w", err)
	}
	wantBase, err := filepath.Abs(spec.BasePath)
	if err != nil {
		return fmt.Errorf("resolve expected backing path: %w", err)
	}
	if filepath.Clean(actualBase) != filepath.Clean(wantBase) {
		return fmt.Errorf("overlay backing file is %q, want %q", info.BackingFilename, spec.BasePath)
	}
	if info.VirtualSize <= 0 {
		return errors.New("overlay virtual size must be positive")
	}
	return nil
}

func (q *QEMUImg) run(parent context.Context, args ...string) ([]byte, error) {
	if q == nil || strings.TrimSpace(q.binary) == "" {
		return nil, errors.New("qemu-img binary must not be empty")
	}
	ctx, cancel := context.WithTimeout(parent, q.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, q.binary, args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("qemu-img timed out after %s", q.timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func validateOverlaySpec(spec OverlaySpec) error {
	if !filepath.IsAbs(spec.Path) || !filepath.IsAbs(spec.BasePath) {
		return errors.New("overlay and base paths must be absolute")
	}
	if filepath.Clean(spec.Path) == filepath.Clean(spec.BasePath) {
		return errors.New("overlay path must differ from base path")
	}
	if spec.BaseFormat != "qcow2" {
		return fmt.Errorf("unsupported overlay base format %q", spec.BaseFormat)
	}
	return nil
}
