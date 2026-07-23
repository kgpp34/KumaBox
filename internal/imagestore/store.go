// SPDX-License-Identifier: MIT

package imagestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/imageimport"
	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

// Store persists image metadata in the KumaBox image index.
type Store struct {
	cloudimgDir string
	engine      meta.MetaEngine
}

var imageIndexCollection = meta.NewCollection[imageIndex]("images", imageIndexTable)

// New returns a Store rooted under rootDir.
func New(rootDir string) *Store {
	cloudimgDir := filepath.Join(rootDir, "cloudimg")
	engine := mustOpenImageEngine(metajson.Namespace{
		Name:     "images",
		FilePath: filepath.Join(cloudimgDir, "index.json"),
		LockPath: filepath.Join(cloudimgDir, "index.lock"),
		Codec:    indexCodec{},
	})
	return NewWithEngine(rootDir, engine)
}

// NewWithEngine creates an image store with an injected metadata engine.
func NewWithEngine(rootDir string, engine meta.MetaEngine) *Store {
	return &Store{cloudimgDir: filepath.Join(rootDir, "cloudimg"), engine: engine}
}

func mustOpenImageEngine(namespace metajson.Namespace) meta.MetaEngine {
	engine, err := metajson.Open(namespace)
	if err != nil {
		panic(fmt.Sprintf("open image metadata engine: %v", err))
	}
	return engine
}

// CreateRequest contains metadata for creating an image record directly.
type CreateRequest struct {
	Name     string
	Source   Source
	RootDisk RootDisk
	Boot     Boot
	OS       OS
	OCI      *OCI
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

// RemoveRequest describes a protected image deletion.
type RemoveRequest struct {
	Ref        string
	Force      bool
	References []Reference
}

// Reference describes a VM that currently references an image.
type Reference struct {
	Kind    string `json:"kind,omitempty"`
	VMID    string `json:"vmId"`
	VMName  string `json:"vmName"`
	VMState string `json:"vmState,omitempty"`
	ImageID string `json:"imageId"`
}

// ImageInUseError reports VM references that blocked image deletion.
type ImageInUseError struct {
	ImageID    string      `json:"imageId"`
	ImageName  string      `json:"imageName"`
	References []Reference `json:"references"`
}

func (e *ImageInUseError) Error() string {
	return fmt.Sprintf("IMAGE_IN_USE: image %s is referenced by %d resource(s)", e.ImageName, len(e.References))
}

func (e *ImageInUseError) Unwrap() error {
	return ErrImageInUse
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
			OCI:           cloneOCI(req.OCI),
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		imageDir := filepath.Join(s.cloudimgDir, id)
		if err := os.MkdirAll(imageDir, 0o755); err != nil {
			return fmt.Errorf("create image dir: %w", err)
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

	stagingDir, cleanup, err := s.createStagingDir("import")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	artifact, err := imageimport.Local(imageimport.Request{
		Source:      sourcePath,
		Destination: filepath.Join(stagingDir, "base.img"),
		QemuImgPath: req.QemuImgPath,
	})
	if err != nil {
		return nil, err
	}
	diskName := "base." + imageimport.DiskExtension(artifact.Format)
	if artifact.Path != filepath.Join(stagingDir, diskName) {
		if err := os.Rename(artifact.Path, filepath.Join(stagingDir, diskName)); err != nil {
			return nil, fmt.Errorf("prepare imported image: %w", err)
		}
		artifact.Path = filepath.Join(stagingDir, diskName)
	}

	return s.commitImportedImage(CreateRequest{
		Name: req.Name,
		Source: Source{
			Type: "local-file",
			URI:  sourcePath,
		},
		RootDisk: RootDisk{
			Path:             diskName,
			Format:           artifact.Format,
			VirtualSizeBytes: artifact.VirtualSizeBytes,
			ActualSizeBytes:  artifact.ActualSizeBytes,
			SHA256:           artifact.SHA256,
		},
		Boot: Boot{
			Mode:     "uefi",
			Firmware: firmwarePath,
		},
		OS: OS{
			Family:  imageimport.OSFamily(sourcePath),
			Profile: "ubuntu-cloudimg",
		},
	}, artifact.Path)
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
	artifact, err := imageimport.Remote(imageimport.Request{
		Source:         req.URL,
		Destination:    downloadedDisk,
		QemuImgPath:    req.QemuImgPath,
		ExpectedSHA256: req.SHA256,
	})
	if err != nil {
		if errors.Is(err, imageimport.ErrChecksumMismatch) {
			return nil, fmt.Errorf("%w: %v", ErrChecksumMismatch, err)
		}
		return nil, err
	}

	diskName := "base." + imageimport.DiskExtension(artifact.Format)
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
			Format:           artifact.Format,
			VirtualSizeBytes: artifact.VirtualSizeBytes,
			ActualSizeBytes:  artifact.ActualSizeBytes,
			SHA256:           artifact.SHA256,
		},
		Boot: Boot{
			Mode:     "uefi",
			Firmware: firmwarePath,
		},
		OS: OS{
			Family:  imageimport.OSFamily(artifact.SourceHint),
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

// Remove deletes an image manifest and managed disk when no VM references it.
func (s *Store) Remove(req RemoveRequest) (*ImageRecord, error) {
	if req.Ref == "" {
		return nil, errors.New("image ref must not be empty")
	}

	var removed *ImageRecord
	err := s.update(func(idx *imageIndex) error {
		id, err := idx.resolve(req.Ref)
		if err != nil {
			return err
		}
		rec := idx.Images[id]
		refs := referencesForImage(req.References, id)
		if len(refs) > 0 {
			return &ImageInUseError{
				ImageID:    rec.ID,
				ImageName:  rec.Name,
				References: refs,
			}
		}

		imageDir := filepath.Join(s.cloudimgDir, id)
		if err := os.RemoveAll(imageDir); err != nil {
			return fmt.Errorf("remove image dir: %w", err)
		}
		delete(idx.Names, rec.Name)
		delete(idx.Images, id)
		removed = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
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
			OCI:       cloneOCI(req.OCI),
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

func cloneOCI(oci *OCI) *OCI {
	if oci == nil {
		return nil
	}
	copied := *oci
	copied.ImageConfig = cloneOCIImageConfig(oci.ImageConfig)
	copied.Layers = append([]OCILayer(nil), oci.Layers...)
	for i := range copied.Layers {
		if copied.Layers[i].EROFS == nil {
			continue
		}
		erofs := *copied.Layers[i].EROFS
		copied.Layers[i].EROFS = &erofs
	}
	return &copied
}

func referencesForImage(refs []Reference, imageID string) []Reference {
	matched := make([]Reference, 0)
	for _, ref := range refs {
		if ref.ImageID == imageID {
			matched = append(matched, ref)
		}
	}
	return matched
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
	ctx := context.Background()
	return s.engine.View(ctx, []meta.Namespace{"images"}, func(reader meta.Reader) error {
		idx, err := s.readIndex(ctx, reader)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func (s *Store) update(fn func(*imageIndex) error) error {
	ctx := context.Background()
	return s.engine.Update(ctx, meta.Scope{Write: "images"}, meta.CommitDurable, func(writer meta.Writer) error {
		idx, err := s.readIndex(ctx, writer)
		if err != nil {
			return err
		}
		if err := fn(idx); err != nil {
			return err
		}
		return imageIndexCollection.Upsert(ctx, writer, imageIndexRecord, idx)
	})
}

func (s *Store) readIndex(ctx context.Context, reader meta.Reader) (*imageIndex, error) {
	idx, err := imageIndexCollection.Get(ctx, reader, imageIndexRecord)
	if errors.Is(err, meta.ErrNotFound) {
		idx = &imageIndex{}
	} else if err != nil {
		return nil, fmt.Errorf("read image index: %w", err)
	}
	idx.init()
	return idx, nil
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
