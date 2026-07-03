// SPDX-License-Identifier: MIT

package imagestore

import (
	"context"
	"crypto/rand"
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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
)

const qemuImgInfoTimeout = 30 * time.Second

// Store persists image metadata in the KumaBox image index.
type Store struct {
	cloudimgDir string
	indexPath   string
	lockPath    string
}

// New returns a Store rooted under rootDir.
func New(rootDir string) *Store {
	cloudimgDir := filepath.Join(rootDir, "cloudimg")
	return &Store{
		cloudimgDir: cloudimgDir,
		indexPath:   filepath.Join(cloudimgDir, "index.json"),
		lockPath:    filepath.Join(cloudimgDir, "index.lock"),
	}
}

// CreateRequest contains metadata for creating an image record directly.
type CreateRequest struct {
	Name     string
	Source   Source
	RootDisk RootDisk
	Boot     Boot
	OS       OS
}

// ImportRequest describes a local cloud image import operation.
type ImportRequest struct {
	Name        string
	File        string
	Firmware    string
	QemuImgPath string
}

// PullRequest describes a URL cloud image pull operation.
type PullRequest struct {
	Name        string
	URL         string
	Firmware    string
	QemuImgPath string
	SHA256      string
}

// Create inserts an image record into the image index.
func (s *Store) Create(req CreateRequest) (*ImageRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *ImageRecord
	err := s.update(func(idx *imageIndex) error {
		if _, ok := idx.Names[req.Name]; ok {
			return fmt.Errorf("%w: %s", ErrNameConflict, req.Name)
		}

		id, err := newID()
		if err != nil {
			return err
		}
		for {
			if _, exists := idx.Images[id]; !exists {
				break
			}
			id, err = newID()
			if err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		rec := &ImageRecord{
			SchemaVersion: "kumabox.image.v1",
			ID:            id,
			Name:          req.Name,
			Source:        req.Source,
			RootDisk:      req.RootDisk,
			Boot:          req.Boot,
			OS:            req.OS,
			CreatedAt:     now,
			UpdatedAt:     now,
		}

		idx.Images[id] = rec
		idx.Names[req.Name] = id
		created = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ImportLocal imports a local cloud image into the managed image store.
func (s *Store) ImportLocal(req ImportRequest) (*ImageRecord, error) {
	if err := validateImportRequest(req); err != nil {
		return nil, err
	}
	sourcePath, err := filepath.Abs(req.File)
	if err != nil {
		return nil, fmt.Errorf("resolve source image path: %w", err)
	}
	firmwarePath, err := filepath.Abs(req.Firmware)
	if err != nil {
		return nil, fmt.Errorf("resolve firmware path: %w", err)
	}
	if err := validateReadableFile(firmwarePath, "firmware"); err != nil {
		return nil, err
	}

	info, err := inspectImage(req.QemuImgPath, sourcePath)
	if err != nil {
		return nil, err
	}

	stagingDir, cleanup, err := s.createStagingDir("import")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	diskName := "base." + diskExtension(info.Format)
	stagedDisk := filepath.Join(stagingDir, diskName)
	sum, actualSize, err := copyWithSHA256(sourcePath, stagedDisk)
	if err != nil {
		return nil, err
	}
	if info.ActualSizeBytes > 0 {
		actualSize = info.ActualSizeBytes
	}

	return s.commitImportedImage(CreateRequest{
		Name: req.Name,
		Source: Source{
			Type: "local-file",
			URI:  sourcePath,
		},
		RootDisk: RootDisk{
			Path:             diskName,
			Format:           info.Format,
			VirtualSizeBytes: info.VirtualSizeBytes,
			ActualSizeBytes:  actualSize,
			SHA256:           sum,
		},
		Boot: Boot{
			Mode:     "uefi",
			Firmware: firmwarePath,
		},
		OS: OS{
			Family:  detectOSFamily(sourcePath),
			Profile: "ubuntu-cloudimg",
		},
	}, stagedDisk)
}

// Pull downloads a cloud image URL into staging and commits it to the image store.
func (s *Store) Pull(req PullRequest) (*ImageRecord, error) {
	if err := validatePullRequest(req); err != nil {
		return nil, err
	}
	firmwarePath, err := filepath.Abs(req.Firmware)
	if err != nil {
		return nil, fmt.Errorf("resolve firmware path: %w", err)
	}
	if err := validateReadableFile(firmwarePath, "firmware"); err != nil {
		return nil, err
	}

	stagingDir, cleanup, err := s.createStagingDir("pull")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	downloadedDisk := filepath.Join(stagingDir, "download.img")
	sum, actualSize, sourceHint, err := downloadToStaging(req.URL, downloadedDisk)
	if err != nil {
		return nil, err
	}
	if err := verifySHA256(req.SHA256, sum); err != nil {
		return nil, err
	}

	info, err := inspectImage(req.QemuImgPath, downloadedDisk)
	if err != nil {
		return nil, err
	}
	if info.ActualSizeBytes > 0 {
		actualSize = info.ActualSizeBytes
	}

	diskName := "base." + diskExtension(info.Format)
	stagedDisk := filepath.Join(stagingDir, diskName)
	if err := os.Rename(downloadedDisk, stagedDisk); err != nil {
		return nil, fmt.Errorf("prepare pulled image: %w", err)
	}

	return s.commitImportedImage(CreateRequest{
		Name: req.Name,
		Source: Source{
			Type: "url",
			URI:  req.URL,
		},
		RootDisk: RootDisk{
			Path:             diskName,
			Format:           info.Format,
			VirtualSizeBytes: info.VirtualSizeBytes,
			ActualSizeBytes:  actualSize,
			SHA256:           sum,
		},
		Boot: Boot{
			Mode:     "uefi",
			Firmware: firmwarePath,
		},
		OS: OS{
			Family:  detectOSFamily(sourceHint),
			Profile: "ubuntu-cloudimg",
		},
	}, stagedDisk)
}

// Inspect returns an image record by exact ID, name, or unique ID prefix.
func (s *Store) Inspect(ref string) (*ImageRecord, error) {
	var rec *ImageRecord
	err := s.withIndex(func(idx *imageIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec = cloneRecord(idx.Images[id])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// List returns all image records sorted by creation time.
func (s *Store) List() ([]*ImageRecord, error) {
	var records []*ImageRecord
	err := s.withIndex(func(idx *imageIndex) error {
		records = make([]*ImageRecord, 0, len(idx.Images))
		for _, rec := range idx.Images {
			records = append(records, cloneRecord(rec))
		}
		sort.Slice(records, func(i, j int) bool {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) commitImportedImage(req CreateRequest, stagedDisk string) (*ImageRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *ImageRecord
	err := s.update(func(idx *imageIndex) error {
		if _, ok := idx.Names[req.Name]; ok {
			return fmt.Errorf("%w: %s", ErrNameConflict, req.Name)
		}

		id, err := newID()
		if err != nil {
			return err
		}
		for {
			if _, exists := idx.Images[id]; !exists {
				break
			}
			id, err = newID()
			if err != nil {
				return err
			}
		}

		imageDir := filepath.Join(s.cloudimgDir, id)
		if err := os.MkdirAll(imageDir, 0o755); err != nil {
			return fmt.Errorf("create image dir: %w", err)
		}
		committedDisk := filepath.Join(imageDir, filepath.Base(req.RootDisk.Path))
		if err := os.Rename(stagedDisk, committedDisk); err != nil {
			return fmt.Errorf("commit root disk: %w", err)
		}

		now := time.Now().UTC()
		rec := &ImageRecord{
			SchemaVersion: "kumabox.image.v1",
			ID:            id,
			Name:          req.Name,
			Source:        req.Source,
			RootDisk: RootDisk{
				Path:             committedDisk,
				Format:           req.RootDisk.Format,
				VirtualSizeBytes: req.RootDisk.VirtualSizeBytes,
				ActualSizeBytes:  req.RootDisk.ActualSizeBytes,
				SHA256:           req.RootDisk.SHA256,
			},
			Boot:      req.Boot,
			OS:        req.OS,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := fileutil.WriteJSONAtomic(filepath.Join(imageDir, "image.json"), rec, ".image-*.tmp"); err != nil {
			_ = os.RemoveAll(imageDir)
			return fmt.Errorf("write image manifest: %w", err)
		}
		if err := fileutil.WriteJSONAtomic(filepath.Join(imageDir, "source.json"), rec.Source, ".source-*.tmp"); err != nil {
			_ = os.RemoveAll(imageDir)
			return fmt.Errorf("write image source manifest: %w", err)
		}

		idx.Images[id] = rec
		idx.Names[req.Name] = id
		created = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) createStagingDir(prefix string) (string, func(), error) {
	stageID, err := newOperationID(prefix)
	if err != nil {
		return "", nil, err
	}
	stagingDir := filepath.Join(s.cloudimgDir, "staging", stageID)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create %s staging dir: %w", prefix, err)
	}
	return stagingDir, func() {
		_ = os.RemoveAll(stagingDir)
	}, nil
}

func (s *Store) withIndex(fn func(*imageIndex) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	idx, err := s.load()
	if err != nil {
		return err
	}
	return fn(idx)
}

func (s *Store) update(fn func(*imageIndex) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	idx, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(idx); err != nil {
		return err
	}
	return s.write(idx)
}

func (s *Store) load() (*imageIndex, error) {
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			idx := &imageIndex{}
			idx.init()
			return idx, nil
		}
		return nil, fmt.Errorf("read image index: %w", err)
	}

	var idx imageIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse image index: %w", err)
	}
	idx.init()
	return &idx, nil
}

func (s *Store) write(idx *imageIndex) error {
	if err := fileutil.WriteJSONAtomic(s.indexPath, idx, ".index-*.tmp"); err != nil {
		return fmt.Errorf("write image index: %w", err)
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create image index lock dir: %w", err)
	}

	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open image index lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock image index: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func validateCreateRequest(req CreateRequest) error {
	if req.Name == "" {
		return errors.New("image name must not be empty")
	}
	return nil
}

func validateImportRequest(req ImportRequest) error {
	if req.Name == "" {
		return errors.New("image name must not be empty")
	}
	if req.File == "" {
		return errors.New("image file must not be empty")
	}
	if req.Firmware == "" {
		return errors.New("firmware must not be empty")
	}
	if req.QemuImgPath == "" {
		return errors.New("qemu-img path must not be empty")
	}
	return nil
}

func validatePullRequest(req PullRequest) error {
	if req.Name == "" {
		return errors.New("image name must not be empty")
	}
	if req.URL == "" {
		return errors.New("image URL must not be empty")
	}
	if req.Firmware == "" {
		return errors.New("firmware must not be empty")
	}
	if req.QemuImgPath == "" {
		return errors.New("qemu-img path must not be empty")
	}
	if req.SHA256 != "" {
		expected := strings.ToLower(strings.TrimSpace(req.SHA256))
		if len(expected) != sha256.Size*2 {
			return errors.New("sha256 must be a 64 character hex digest")
		}
		if _, err := hex.DecodeString(expected); err != nil {
			return fmt.Errorf("sha256 must be hex: %w", err)
		}
	}
	return nil
}

func validateReadableFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", label, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s must be a file: %s", label, path)
	}
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open %s: %w", label, err)
	}
	return file.Close()
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate image ID: %w", err)
	}
	return "img_" + hex.EncodeToString(raw[:]), nil
}

func newOperationID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}

type qemuImageInfo struct {
	Filename         string `json:"filename"`
	Format           string `json:"format"`
	VirtualSizeBytes int64  `json:"virtual-size"`
	ActualSizeBytes  int64  `json:"actual-size"`
}

func inspectImage(qemuImgPath, sourcePath string) (*qemuImageInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qemuImgInfoTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, qemuImgPath, "info", "--output=json", sourcePath).Output() //nolint:gosec
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("qemu-img info %s timed out after %s", sourcePath, qemuImgInfoTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("qemu-img info %s: %w", sourcePath, err)
	}
	var info qemuImageInfo
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

func copyWithSHA256(src, dst string) (string, int64, error) {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return "", 0, fmt.Errorf("open source image: %w", err)
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return "", 0, fmt.Errorf("create staged image: %w", err)
	}

	hasher := sha256.New()
	size, copyErr := copyAndHash(out, in, hasher)
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("close staged image: %w", closeErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func downloadToStaging(rawURL, dst string) (string, int64, string, error) {
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
		sum, size, err := copyWithSHA256(sourcePath, dst)
		return sum, size, sourcePath, err
	case "http", "https":
		sum, size, err := downloadHTTPToFile(rawURL, dst)
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

func downloadHTTPToFile(rawURL, dst string) (string, int64, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("create image download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

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

func diskExtension(format string) string {
	switch strings.ToLower(format) {
	case "raw":
		return "raw"
	case "qcow2":
		return "qcow2"
	default:
		return "img"
	}
}

func detectOSFamily(path string) string {
	lower := strings.ToLower(filepath.Base(path))
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "jammy") || strings.Contains(lower, "noble") {
		return "ubuntu"
	}
	return ""
}
