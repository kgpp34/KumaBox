// SPDX-License-Identifier: MIT

package ocistore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	"github.com/kumabox/kumabox/internal/ociresolver"
	"github.com/kumabox/kumabox/internal/ocisource"
)

// Store caches OCI manifest/config/layer blobs by digest.
type Store struct {
	rootDir  string
	engine   meta.MetaEngine
	blobsDir string
	stageDir string
}

// PullRequest describes a P3-02 content-store pull.
type PullRequest struct {
	Ref      string
	Platform string
	Source   string
	Progress func(ProgressEvent)
}

// ProgressEvent reports one durable phase of an OCI import.
type ProgressEvent struct {
	Phase  string
	Index  int
	Total  int
	Digest string
	Cached bool
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
	Source        string               `json:"source"`
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

var contentIndexCollection = meta.NewCollection[indexFile]("oci-content", contentIndexTable)

// New returns an OCI content store under rootDir.
func New(rootDir string) *Store {
	base := filepath.Join(rootDir, "oci", "content")
	return NewWithEngine(rootDir, mustOpenContentEngine(metajson.Namespace{Name: "oci-content", FilePath: filepath.Join(base, "index.json"), LockPath: filepath.Join(base, "index.lock"), Codec: indexCodec{}}))
}

// NewWithEngine creates an OCI content store with an injected metadata engine.
func NewWithEngine(rootDir string, engine meta.MetaEngine) *Store {
	base := filepath.Join(rootDir, "oci", "content")
	return &Store{rootDir: base, engine: engine, blobsDir: filepath.Join(base, "blobs"), stageDir: filepath.Join(base, "staging")}
}

// MetadataEngine exposes the persistence boundary to migration tools.
func (s *Store) MetadataEngine() meta.MetaEngine { return s.engine }

func mustOpenContentEngine(namespace metajson.Namespace) meta.MetaEngine {
	engine, err := metajson.Open(namespace)
	if err != nil {
		panic(fmt.Sprintf("open OCI content metadata engine: %v", err))
	}
	return engine
}

// Pull resolves an OCI ref and downloads manifest/config/layers into the blob store.
func (s *Store) Pull(ctx context.Context, req PullRequest) (*PullResult, error) {
	if req.Ref == "" {
		return nil, fmt.Errorf("OCI_REF_REQUIRED: ref must not be empty")
	}

	sourceResult, err := ocisource.Open(ctx, ocisource.Request{
		Ref:      req.Ref,
		Platform: req.Platform,
		Source:   req.Source,
	})
	if err != nil {
		return nil, err
	}
	img := sourceResult.Image
	resolved := sourceResult.Resolved
	source := sourceResult.Source

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

	result := &PullResult{
		SchemaVersion: "kumabox.oci.content.pull.v1",
		Ref:           resolved.Ref,
		Source:        source,
		DigestRef:     resolved.DigestRef,
		Platform:      resolved.Platform,
	}
	emitProgress(req.Progress, ProgressEvent{Phase: "manifest", Digest: resolved.ResolvedDigest})

	err = s.engine.Update(ctx, meta.Scope{Write: "oci-content"}, meta.CommitDurable, func(writer meta.Writer) error {
		idx, err := contentIndexCollection.Get(ctx, writer, contentIndexRecord)
		if errors.Is(err, meta.ErrNotFound) {
			idx = &indexFile{}
		} else if err != nil {
			return fmt.Errorf("read OCI content index: %w", err)
		}
		idx.init()
		result.Manifest, err = s.ensureBlob(idx, resolved.ResolvedDigest, "application/vnd.oci.image.manifest.v1+json", bytes.NewReader(manifestBytes))
		if err != nil {
			return fmt.Errorf("store manifest: %w", err)
		}
		result.Config, err = s.ensureBlob(idx, resolved.Config.Digest, resolved.Config.MediaType, bytes.NewReader(configBytes))
		if err != nil {
			return fmt.Errorf("store config: %w", err)
		}
		emitProgress(req.Progress, ProgressEvent{Phase: "config", Digest: result.Config.Digest})

		for i, layer := range layers {
			digest, err := layer.Digest()
			if err != nil {
				return fmt.Errorf("layer %d digest: %w", i, err)
			}
			mediaType, err := layer.MediaType()
			if err != nil {
				return fmt.Errorf("layer %d media type: %w", i, err)
			}
			rc, err := layer.Compressed()
			if err != nil {
				return fmt.Errorf("layer %d compressed stream: %w", i, err)
			}
			rec, storeErr := s.ensureBlob(idx, digest.String(), string(mediaType), rc)
			closeErr := rc.Close()
			if storeErr != nil {
				return fmt.Errorf("store layer %d: %w", i, storeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close layer %d: %w", i, closeErr)
			}
			result.Layers = append(result.Layers, rec)
			emitProgress(req.Progress, ProgressEvent{
				Phase:  "layer",
				Index:  i,
				Total:  len(layers),
				Digest: rec.Digest,
				Cached: rec.CreatedAt != rec.UpdatedAt,
			})
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
		return contentIndexCollection.Upsert(ctx, writer, contentIndexRecord, idx)
	})
	if err != nil {
		return nil, err
	}
	emitProgress(req.Progress, ProgressEvent{Phase: "complete", Total: len(result.Layers)})
	return result, nil
}

func emitProgress(progress func(ProgressEvent), event ProgressEvent) {
	if progress != nil {
		progress(event)
	}
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
			if got, hashErr := fileSHA256(existing.Path); hashErr == nil && got == hexDigest {
				existing.UpdatedAt = now
				return *existing, nil
			}
		}
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		got, hashErr := fileSHA256(path)
		if hashErr != nil {
			return BlobRecord{}, fmt.Errorf("verify existing blob: %w", hashErr)
		}
		if got != hexDigest {
			if err := os.Remove(path); err != nil {
				return BlobRecord{}, fmt.Errorf("remove corrupt blob: %w", err)
			}
		} else {
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
		if !os.IsExist(err) {
			return BlobRecord{}, fmt.Errorf("commit blob: %w", err)
		}
		if got, hashErr := fileSHA256(path); hashErr != nil || got != hexDigest {
			return BlobRecord{}, fmt.Errorf("commit blob: existing target failed digest verification")
		}
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

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
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
