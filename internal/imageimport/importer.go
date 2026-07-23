// SPDX-License-Identifier: MIT

// Package imageimport acquires and inspects local or remote cloud images.
//
// It deliberately does not know about KumaBox image records or indexes. The
// caller owns the staging directory and decides how the resulting artifact is
// persisted.
package imageimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const qemuImgInfoTimeout = 30 * time.Second

// ErrChecksumMismatch reports a downloaded image that does not match the
// caller's expected SHA-256 digest.
var ErrChecksumMismatch = errors.New("image checksum mismatch")

// Request contains the paths and tooling needed to acquire a disk image.
type Request struct {
	Source         string
	Destination    string
	QemuImgPath    string
	ExpectedSHA256 string
}

// Artifact describes an acquired and inspected disk image.
type Artifact struct {
	Path             string
	SourceHint       string
	SHA256           string
	SizeBytes        int64
	Format           string
	VirtualSizeBytes int64
	ActualSizeBytes  int64
}

// Local copies and inspects a local image into req.Destination.
func Local(req Request) (*Artifact, error) {
	if req.Source == "" {
		return nil, errors.New("source image path must not be empty")
	}
	if req.Destination == "" {
		return nil, errors.New("destination image path must not be empty")
	}

	sourcePath, err := filepath.Abs(req.Source)
	if err != nil {
		return nil, fmt.Errorf("resolve source image path: %w", err)
	}
	info, err := inspect(req.QemuImgPath, sourcePath)
	if err != nil {
		return nil, err
	}
	sum, size, err := copyAndHashFile(sourcePath, req.Destination)
	if err != nil {
		return nil, err
	}
	return artifact(req.Destination, sourcePath, sum, size, info), nil
}

// Remote downloads and inspects an HTTP(S) or file URL into req.Destination.
func Remote(req Request) (*Artifact, error) {
	if req.Source == "" {
		return nil, errors.New("image URL must not be empty")
	}
	if req.Destination == "" {
		return nil, errors.New("destination image path must not be empty")
	}

	sum, size, sourceHint, err := download(req.Source, req.Destination)
	if err != nil {
		return nil, err
	}
	if err := verifySHA256(req.ExpectedSHA256, sum); err != nil {
		return nil, err
	}
	info, err := inspect(req.QemuImgPath, req.Destination)
	if err != nil {
		return nil, err
	}
	return artifact(req.Destination, sourceHint, sum, size, info), nil
}

type imageInfo struct {
	Format           string `json:"format"`
	VirtualSizeBytes int64  `json:"virtual-size"`
	ActualSizeBytes  int64  `json:"actual-size"`
}

func artifact(path, sourceHint, sum string, size int64, info *imageInfo) *Artifact {
	actualSize := info.ActualSizeBytes
	if actualSize <= 0 {
		actualSize = size
	}
	return &Artifact{
		Path:             path,
		SourceHint:       sourceHint,
		SHA256:           sum,
		SizeBytes:        size,
		Format:           info.Format,
		VirtualSizeBytes: info.VirtualSizeBytes,
		ActualSizeBytes:  actualSize,
	}
}

func inspect(qemuImgPath, sourcePath string) (*imageInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qemuImgInfoTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, qemuImgPath, "info", "--output=json", sourcePath).Output() //nolint:gosec
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("qemu-img info %s timed out after %s", sourcePath, qemuImgInfoTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("qemu-img info %s: %w", sourcePath, err)
	}
	var info imageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse qemu-img info: %w", err)
	}
	if info.Format == "" {
		return nil, errors.New("qemu-img info did not report image format")
	}
	if info.VirtualSizeBytes < 0 || info.ActualSizeBytes < 0 {
		return nil, errors.New("qemu-img info reported negative image size")
	}
	return &info, nil
}

func copyAndHashFile(src, dst string) (sum string, size int64, err error) {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return "", 0, fmt.Errorf("open source image: %w", err)
	}
	defer func() {
		if closeErr := in.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close source image: %w", closeErr)
		}
	}()
	return writeStreamWithSHA256(in, dst)
}

func download(rawURL, dst string) (string, int64, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, "", fmt.Errorf("parse image URL: %w", err)
	}
	switch parsed.Scheme {
	case "file":
		sourcePath, err := fileURLPath(parsed)
		if err != nil {
			return "", 0, "", err
		}
		sum, size, err := copyAndHashFile(sourcePath, dst)
		return sum, size, sourcePath, err
	case "http", "https":
		sum, size, err := downloadHTTP(rawURL, dst)
		return sum, size, parsed.Path, err
	default:
		return "", 0, "", fmt.Errorf("unsupported image URL scheme: %s", parsed.Scheme)
	}
}

func fileURLPath(parsed *url.URL) (string, error) {
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("unsupported file URL host: %s", parsed.Host)
	}
	if parsed.Path == "" {
		return "", errors.New("file URL path must not be empty")
	}
	path, err := url.PathUnescape(parsed.Path)
	if err != nil {
		return "", fmt.Errorf("decode file URL path: %w", err)
	}
	return path, nil
}

func downloadHTTP(rawURL, dst string) (sum string, size int64, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("create image download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download image: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close image response: %w", closeErr)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", 0, fmt.Errorf("download image: unexpected HTTP status %s", resp.Status)
	}
	return writeStreamWithSHA256(resp.Body, dst)
}

func writeStreamWithSHA256(src io.Reader, dst string) (string, int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return "", 0, fmt.Errorf("create staged image: %w", err)
	}
	hasher := sha256.New()
	size, copyErr := copyAndHash(out, src, hasher)
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("close staged image: %w", closeErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func verifySHA256(expected, actual string) error {
	if expected == "" {
		return nil
	}
	normalized := strings.ToLower(strings.TrimSpace(expected))
	if normalized != actual {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, actual, normalized)
	}
	return nil
}

func copyAndHash(dst io.Writer, src io.Reader, hasher hash.Hash) (int64, error) {
	size, err := io.Copy(io.MultiWriter(dst, hasher), src)
	if err != nil {
		return 0, fmt.Errorf("copy image to staging: %w", err)
	}
	return size, nil
}

// DiskExtension returns the managed filename extension for an image format.
func DiskExtension(format string) string {
	switch strings.ToLower(format) {
	case "raw":
		return "raw"
	case "qcow2":
		return "qcow2"
	default:
		return "img"
	}
}

// OSFamily infers a guest family from a source filename.
func OSFamily(path string) string {
	lower := strings.ToLower(filepath.Base(path))
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "jammy") || strings.Contains(lower, "noble") {
		return "ubuntu"
	}
	return ""
}
