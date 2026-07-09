// SPDX-License-Identifier: MIT

package ocistore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/ociresolver"
)

// Store caches OCI manifest/config/layer blobs by digest.
type Store struct {
	rootDir  string
	index    string
	lockPath string
	blobsDir string
	stageDir string
}

// PullRequest describes a P3-02 content-store pull.
type PullRequest struct {
	Ref      string
	Platform string
}

// BlobRecord is one content-addressed blob on disk.
type BlobRecord struct {
	Digest    string    `json:"digest"`
	Path      string    `json:"path"`
	MediaType string    `json:"mediaType"`
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RefRecord records the latest digest resolved for a tag/ref.
type RefRecord struct {
	Ref            string               `json:"ref"`
	DigestRef      string               `json:"digestRef"`
	ResolvedDigest string               `json:"resolvedDigest"`
	Platform       ociresolver.Platform `json:"platform"`
	Config         string               `json:"config"`
	Layers         []string             `json:"layers"`
	UpdatedAt      time.Time            `json:"updatedAt"`
}

// PullResult summarizes a content-store pull.
type PullResult struct {
	SchemaVersion string               `json:"schemaVersion"`
	Ref           string               `json:"ref"`
	DigestRef     string               `json:"digestRef"`
	Platform      ociresolver.Platform `json:"platform"`
	Manifest      BlobRecord           `json:"manifest"`
	Config        BlobRecord           `json:"config"`
	Layers        []BlobRecord         `json:"layers"`
	Cached        int                  `json:"cached"`
	Downloaded    int                  `json:"downloaded"`
}

type indexFile struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Blobs         map[string]*BlobRecord `json:"blobs"`
	Refs          map[string]*RefRecord  `json:"refs"`
}

// New returns an OCI content store under rootDir.
func New(rootDir string) *Store {
	base := filepath.Join(rootDir, "oci", "content")
	return &Store{
		rootDir:  base,
		index:    filepath.Join(base, "index.json"),
		lockPath: filepath.Join(base, "index.lock"),
		blobsDir: filepath.Join(base, "blobs"),
		stageDir: filepath.Join(base, "staging"),
	}
}

// Pull resolves an OCI ref and downloads manifest/config/layers into the blob store.
func (s *Store) Pull(ctx context.Context, req PullRequest) (*PullResult, error) {
	if req.Ref == "" {
		return nil, fmt.Errorf("OCI_REF_REQUIRED: ref must not be empty")
	}

	resolved, err := (ociresolver.Resolver{}).Resolve(ctx, req.Ref, req.Platform)
	if err != nil {
		return nil, err
	}
	parsed, err := name.ParseReference(req.Ref)
	if err != nil {
		return nil, fmt.Errorf("OCI_REF_INVALID: %w", err)
	}
	img, err := remote.Image(parsed,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(resolved.Platform.V1()),
	)
	if err != nil {
		return nil, fmt.Errorf("OCI_PULL_FAILED: %w", err)
	}

	manifestBytes, err := img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("OCI_MANIFEST_FAILED: %w", err)
	}
	configBytes, err := img.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("OCI_CONFIG_FAILED: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("OCI_LAYERS_FAILED: %w", err)
	}

	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	idx, err := s.load()
	if err != nil {
		return nil, err
	}

	result := &PullResult{
		SchemaVersion: "kumabox.oci.content.pull.v1",
		Ref:           resolved.Ref,
		DigestRef:     resolved.DigestRef,
		Platform:      resolved.Platform,
	}

	result.Manifest, err = s.ensureBlob(idx, resolved.ResolvedDigest, "application/vnd.oci.image.manifest.v1+json", bytes.NewReader(manifestBytes))
	if err != nil {
		return nil, fmt.Errorf("store manifest: %w", err)
	}
	result.Config, err = s.ensureBlob(idx, resolved.Config.Digest, resolved.Config.MediaType, bytes.NewReader(configBytes))
	if err != nil {
		return nil, fmt.Errorf("store config: %w", err)
	}

	for i, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return nil, fmt.Errorf("layer %d digest: %w", i, err)
		}
		mediaType, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("layer %d media type: %w", i, err)
		}
		rc, err := layer.Compressed()
		if err != nil {
			return nil, fmt.Errorf("layer %d compressed stream: %w", i, err)
		}
		rec, storeErr := s.ensureBlob(idx, digest.String(), string(mediaType), rc)
		closeErr := rc.Close()
		if storeErr != nil {
			return nil, fmt.Errorf("store layer %d: %w", i, storeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close layer %d: %w", i, closeErr)
		}
		result.Layers = append(result.Layers, rec)
	}

	for _, rec := range append([]BlobRecord{result.Manifest, result.Config}, result.Layers...) {
		if rec.CreatedAt.Equal(rec.UpdatedAt) {
			result.Downloaded++
			continue
		}
		result.Cached++
	}

	layerDigests := make([]string, 0, len(result.Layers))
	for _, layer := range result.Layers {
		layerDigests = append(layerDigests, layer.Digest)
	}
	now := time.Now().UTC()
	idx.Refs[resolved.Ref] = &RefRecord{
		Ref:            resolved.Ref,
		DigestRef:      resolved.DigestRef,
		ResolvedDigest: resolved.ResolvedDigest,
		Platform:       resolved.Platform,
		Config:         result.Config.Digest,
		Layers:         layerDigests,
		UpdatedAt:      now,
	}
	if err := s.write(idx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ensureBlob(idx *indexFile, digest, mediaType string, src io.Reader) (BlobRecord, error) {
	algo, hexDigest, err := splitDigest(digest)
	if err != nil {
		return BlobRecord{}, err
	}
	path := filepath.Join(s.blobsDir, algo, hexDigest)
	now := time.Now().UTC()

	if existing, ok := idx.Blobs[digest]; ok {
		if info, err := os.Stat(existing.Path); err == nil && info.Mode().IsRegular() {
			existing.UpdatedAt = now
			return *existing, nil
		}
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		rec := &BlobRecord{
			Digest:    digest,
			Path:      path,
			MediaType: mediaType,
			SizeBytes: info.Size(),
			CreatedAt: now,
			UpdatedAt: now,
		}
		idx.Blobs[digest] = rec
		return *rec, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return BlobRecord{}, fmt.Errorf("create blob dir: %w", err)
	}
	if err := os.MkdirAll(s.stageDir, 0o755); err != nil {
		return BlobRecord{}, fmt.Errorf("create staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.stageDir, "blob-*")
	if err != nil {
		return BlobRecord{}, fmt.Errorf("create staging blob: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	hasher := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(src, hasher))
	if err != nil {
		_ = tmp.Close()
		return BlobRecord{}, fmt.Errorf("write staging blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return BlobRecord{}, fmt.Errorf("sync staging blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return BlobRecord{}, fmt.Errorf("close staging blob: %w", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != hexDigest {
		return BlobRecord{}, fmt.Errorf("OCI_DIGEST_MISMATCH: %s got sha256:%s", digest, got)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return BlobRecord{}, fmt.Errorf("commit blob: %w", err)
	}

	rec := &BlobRecord{
		Digest:    digest,
		Path:      path,
		MediaType: mediaType,
		SizeBytes: n,
		CreatedAt: now,
		UpdatedAt: now,
	}
	idx.Blobs[digest] = rec
	return *rec, nil
}

func (s *Store) load() (*indexFile, error) {
	raw, err := os.ReadFile(s.index) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			idx := &indexFile{}
			idx.init()
			return idx, nil
		}
		return nil, fmt.Errorf("read OCI content index: %w", err)
	}
	var idx indexFile
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse OCI content index: %w", err)
	}
	idx.init()
	return &idx, nil
}

func (s *Store) write(idx *indexFile) error {
	if err := fileutil.WriteJSONAtomic(s.index, idx, ".index-*.tmp"); err != nil {
		return fmt.Errorf("write OCI content index: %w", err)
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create OCI content lock dir: %w", err)
	}
	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open OCI content lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock OCI content index: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (idx *indexFile) init() {
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = "kumabox.oci.content.index.v1"
	}
	if idx.Blobs == nil {
		idx.Blobs = make(map[string]*BlobRecord)
	}
	if idx.Refs == nil {
		idx.Refs = make(map[string]*RefRecord)
	}
}

func splitDigest(digest string) (string, string, error) {
	algo, value, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || value == "" {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %s", digest)
	}
	if algo != "sha256" {
		return "", "", fmt.Errorf("OCI_DIGEST_UNSUPPORTED: %s", digest)
	}
	if len(value) != sha256.Size*2 {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %s", digest)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %w", err)
	}
	return algo, value, nil
}
