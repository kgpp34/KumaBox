// SPDX-License-Identifier: MIT

// Package ocibuild builds KumaBox image manifests from OCI images.
package ocibuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"

	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const ociCmdlineTemplate = "console=ttyS0 loglevel=3 clocksource=kvm-clock reboot=k panic=1 boot=kumabox-overlay kumabox.layers={{layers}} kumabox.cow={{cow}} kumabox.timeout=10 rw"

// BuildRequest describes an OCI image build.
type BuildRequest struct {
	Name        string
	Ref         string
	Platform    string
	Source      string
	MkfsEROFS   string
	Concurrency int
	Progress    func(ocistore.ProgressEvent)
}

// Builder converts OCI layers into shared EROFS blobs and publishes an image record.
type Builder struct {
	rootDir  string
	erofsDir string
	stageDir string
	content  interface {
		Pull(context.Context, ocistore.PullRequest) (*ocistore.PullResult, error)
	}
	images interface {
		Create(imagestore.CreateRequest) (*imagestore.ImageRecord, error)
	}
}

// New returns a Builder rooted under rootDir.
func New(rootDir string) *Builder {
	return NewWithStores(rootDir, ocistore.New(rootDir), imagestore.New(rootDir))
}

// NewWithStores creates a builder using caller-owned metadata stores.
func NewWithStores(rootDir string, content interface {
	Pull(context.Context, ocistore.PullRequest) (*ocistore.PullResult, error)
}, images interface {
	Create(imagestore.CreateRequest) (*imagestore.ImageRecord, error)
}) *Builder {
	base := filepath.Join(rootDir, "oci", "erofs")
	return &Builder{
		rootDir:  rootDir,
		erofsDir: filepath.Join(base, "blobs"),
		stageDir: filepath.Join(base, "staging"),
		content:  content,
		images:   images,
	}
}

// Build pulls an OCI image, converts its layers to EROFS, and records image metadata.
func (b *Builder) Build(ctx context.Context, req BuildRequest) (*imagestore.ImageRecord, error) {
	if req.Name == "" {
		return nil, errors.New("image name must not be empty")
	}
	if req.Ref == "" {
		return nil, errors.New("OCI ref must not be empty")
	}
	if req.MkfsEROFS == "" {
		req.MkfsEROFS = "mkfs.erofs"
	}

	pull, err := b.content.Pull(ctx, ocistore.PullRequest{
		Ref:      req.Ref,
		Platform: req.Platform,
		Source:   req.Source,
		Progress: req.Progress,
	})
	if err != nil {
		return nil, err
	}

	if err := checkEROFSVersion(ctx, req.MkfsEROFS); err != nil {
		return nil, err
	}
	results := make([]layerBuildResult, len(pull.Layers))
	workers := req.Concurrency
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(pull.Layers) {
		workers = len(pull.Layers)
	}
	if workers == 0 {
		return nil, fmt.Errorf("BOOT_PROFILE_UNSUPPORTED: OCI image has no layers")
	}
	group, groupCtx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, workers)
	for i, layer := range pull.Layers {
		i, layer := i, layer
		group.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-groupCtx.Done():
				return groupCtx.Err()
			}
			defer func() { <-sem }()
			erofs, kernel, initrd, err := b.ensureEROFSWithAssets(groupCtx, req.MkfsEROFS, layer)
			if err != nil {
				return fmt.Errorf("build layer %d %s: %w", i, layer.Digest, err)
			}
			results[i] = layerBuildResult{
				layer: imagestore.OCILayer{
					Index: i, Digest: layer.Digest, Serial: vmstore.LayerSerial(i),
					MediaType: layer.MediaType, SizeBytes: layer.SizeBytes, EROFS: erofs,
				},
				kernel: kernel, initrd: initrd,
			}
			if kernel != nil {
				results[i].layer.Kernel = kernel.Path
			}
			if initrd != nil {
				results[i].layer.Initrd = initrd.Path
			}
			if req.Progress != nil {
				req.Progress(ocistore.ProgressEvent{Phase: "erofs", Index: i, Total: len(pull.Layers), Digest: layer.Digest})
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	layers := make([]imagestore.OCILayer, 0, len(results))
	var kernel, initrd *bootAsset
	for _, result := range results {
		layers = append(layers, result.layer)
		if result.kernel != nil {
			kernel = result.kernel
		}
		if result.initrd != nil {
			initrd = result.initrd
		}
	}
	if kernel == nil || initrd == nil {
		return nil, fmt.Errorf("BOOT_PROFILE_UNSUPPORTED: OCI image must contain /boot/vmlinuz-* and /boot/initrd.img-*")
	}
	boot := imagestore.Boot{Mode: "direct", Kernel: kernel.Path, Initrd: initrd.Path, Cmdline: ociCmdlineTemplate}
	imageConfig, err := decodeOCIImageConfig(pull.Config.Path)
	if err != nil {
		return nil, err
	}

	return b.images.Create(imagestore.CreateRequest{
		Name: req.Name,
		Source: imagestore.Source{
			Type: "oci",
			URI:  pull.DigestRef,
		},
		OS: imagestore.OS{
			Family:  "linux",
			Profile: "oci-erofs",
		},
		Boot: boot,
		OCI: &imagestore.OCI{
			Ref:       pull.Ref,
			Source:    pull.Source,
			DigestRef: pull.DigestRef,
			Platform: imagestore.OCIPlatform{
				OS:           pull.Platform.OS,
				Architecture: pull.Platform.Architecture,
				Variant:      pull.Platform.Variant,
			},
			Config: imagestore.OCIDescriptor{
				Digest:    pull.Config.Digest,
				MediaType: pull.Config.MediaType,
				SizeBytes: pull.Config.SizeBytes,
			},
			ImageConfig:    imageConfig,
			AgentInjection: "deferred",
			Layers:         layers,
			BuiltAt:        time.Now().UTC(),
		},
	})
}

type layerBuildResult struct {
	layer  imagestore.OCILayer
	kernel *bootAsset
	initrd *bootAsset
}

func decodeOCIImageConfig(configPath string) (imagestore.OCIImageConfig, error) {
	raw, err := os.ReadFile(configPath) //nolint:gosec
	if err != nil {
		return imagestore.OCIImageConfig{}, fmt.Errorf("read OCI config: %w", err)
	}
	var doc struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return imagestore.OCIImageConfig{}, fmt.Errorf("decode OCI config: %w", err)
	}
	if len(doc.Config) == 0 {
		return imagestore.OCIImageConfig{}, nil
	}

	cfg := imagestore.OCIImageConfig{}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Env"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.Env = value
	}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Cmd"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.Cmd = value
	}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Entrypoint"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.Entrypoint = value
	}
	if value, ok, err := decodeConfigString(doc.Config, "WorkingDir"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.Workdir = value
	}
	if value, ok, err := decodeConfigString(doc.Config, "User"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.User = value
	}
	if value, ok, err := decodeConfigLabels(doc.Config, "Labels"); err != nil {
		return imagestore.OCIImageConfig{}, err
	} else if ok {
		cfg.Labels = value
	}
	return cfg, nil
}

func decodeConfigStringSlice(config map[string]json.RawMessage, key string) (*[]string, bool, error) {
	raw, ok := config[key]
	if !ok {
		return nil, false, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, fmt.Errorf("decode OCI config %s: %w", key, err)
	}
	return &values, true, nil
}

func decodeConfigString(config map[string]json.RawMessage, key string) (*string, bool, error) {
	raw, ok := config[key]
	if !ok {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, fmt.Errorf("decode OCI config %s: %w", key, err)
	}
	return &value, true, nil
}

func decodeConfigLabels(config map[string]json.RawMessage, key string) (*map[string]string, bool, error) {
	raw, ok := config[key]
	if !ok {
		return nil, false, nil
	}
	var labels map[string]string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, false, fmt.Errorf("decode OCI config %s: %w", key, err)
	}
	return &labels, true, nil
}

func (b *Builder) resolveBootProfile(layers []ocistore.BlobRecord) (imagestore.Boot, error) {
	var kernel *bootAsset
	var initrd *bootAsset

	for _, layer := range layers {
		layerKernel, layerInitrd, err := b.scanBootAssets(layer)
		if err != nil {
			return imagestore.Boot{}, err
		}
		if layerKernel != nil {
			kernel = layerKernel
		}
		if layerInitrd != nil {
			initrd = layerInitrd
		}
	}
	if kernel == nil || initrd == nil {
		return imagestore.Boot{}, fmt.Errorf("BOOT_PROFILE_UNSUPPORTED: OCI image must contain /boot/vmlinuz-* and /boot/initrd.img-*")
	}
	return imagestore.Boot{
		Mode:    "direct",
		Kernel:  kernel.Path,
		Initrd:  initrd.Path,
		Cmdline: ociCmdlineTemplate,
	}, nil
}

func (b *Builder) scanBootAssets(layer ocistore.BlobRecord) (*bootAsset, *bootAsset, error) {
	in, err := os.Open(layer.Path) //nolint:gosec
	if err != nil {
		return nil, nil, fmt.Errorf("open layer blob: %w", err)
	}
	defer in.Close() //nolint:errcheck

	reader, closeReader, err := layerTarReader(layer.MediaType, in)
	if err != nil {
		return nil, nil, err
	}
	defer closeReader()

	var kernel *bootAsset
	var initrd *bootAsset
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read layer tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		kind, ok := bootAssetKind(hdr.Name)
		if !ok {
			continue
		}
		asset, err := b.commitBootAsset(kind, hdr.Name, layer.Digest, tr)
		if err != nil {
			return nil, nil, err
		}
		switch kind {
		case "kernel":
			if kernel == nil || asset.SourcePath > kernel.SourcePath {
				kernel = asset
			}
		case "initrd":
			if initrd == nil || asset.SourcePath > initrd.SourcePath {
				initrd = asset
			}
		}
	}
	return kernel, initrd, nil
}

type bootAsset struct {
	Path        string
	Digest      string
	SizeBytes   int64
	SourceLayer string
	SourcePath  string
}

func bootAssetKind(name string) (string, bool) {
	cleaned := strings.TrimPrefix(path.Clean(strings.TrimPrefix(name, "/")), "./")
	dir := path.Dir(cleaned)
	base := path.Base(cleaned)
	if dir != "boot" && dir != "." {
		return "", false
	}
	if base == "vmlinuz" || strings.HasPrefix(base, "vmlinuz-") {
		return "kernel", true
	}
	if base == "initrd.img" || strings.HasPrefix(base, "initrd.img-") || strings.HasPrefix(base, "initramfs-") {
		return "initrd", true
	}
	return "", false
}

func (b *Builder) commitBootAsset(kind, sourcePath, sourceLayer string, src io.Reader) (*bootAsset, error) {
	opID, err := operationID()
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(b.stageDir, "boot-"+opID)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, fmt.Errorf("create boot asset staging dir: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck

	tmpPath := filepath.Join(stage, kind)
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("create boot asset staging file: %w", err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(tmp, io.TeeReader(src, hasher))
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("write boot asset staging file: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close boot asset staging file: %w", closeErr)
	}

	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	_, value, err := splitDigest(digest)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(b.rootDir, "oci", "boot", "blobs", "sha256", value)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("create boot asset dir: %w", err)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpPath, target); err != nil {
			if !os.IsExist(err) {
				return nil, fmt.Errorf("commit boot asset: %w", err)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat boot asset: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("stat committed boot asset: %w", err)
	}
	return &bootAsset{
		Path:        target,
		Digest:      digest,
		SizeBytes:   info.Size(),
		SourceLayer: sourceLayer,
		SourcePath:  strings.TrimPrefix(path.Clean(strings.TrimPrefix(sourcePath, "/")), "./"),
	}, nil
}

func (b *Builder) ensureEROFS(ctx context.Context, mkfs string, layer ocistore.BlobRecord) (*imagestore.EROFSLayer, error) {
	erofs, _, _, err := b.ensureEROFSWithAssets(ctx, mkfs, layer)
	return erofs, err
}

func (b *Builder) ensureEROFSWithAssets(ctx context.Context, mkfs string, layer ocistore.BlobRecord) (*imagestore.EROFSLayer, *bootAsset, *bootAsset, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		erofs, kernel, initrd, err := b.ensureEROFSOnce(ctx, mkfs, layer)
		if err == nil {
			return erofs, kernel, initrd, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, nil, nil, fmt.Errorf("EROFS_CONVERSION_FAILED after retries: %w", lastErr)
}

func (b *Builder) ensureEROFSOnce(ctx context.Context, mkfs string, layer ocistore.BlobRecord) (*imagestore.EROFSLayer, *bootAsset, *bootAsset, error) {
	algo, value, err := splitDigest(layer.Digest)
	if err != nil {
		return nil, nil, nil, err
	}
	target := filepath.Join(b.erofsDir, algo, value+".erofs")
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		sum, err := fileSHA256(target)
		if err != nil {
			return nil, nil, nil, err
		}
		kernel, initrd, scanErr := b.scanBootAssets(layer)
		if scanErr != nil {
			return nil, nil, nil, scanErr
		}
		return &imagestore.EROFSLayer{
			Path:        target,
			Filesystem:  "erofs",
			Digest:      "sha256:" + sum,
			SizeBytes:   info.Size(),
			SourceLayer: layer.Digest,
		}, kernel, initrd, nil
	}

	opID, err := operationID()
	if err != nil {
		return nil, nil, nil, err
	}
	stage := filepath.Join(b.stageDir, "erofs-"+opID)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("create EROFS staging dir: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck

	stagedEROFS := filepath.Join(stage, "layer.erofs")
	cmd := exec.CommandContext(ctx, mkfs, "--tar=f", "-zlz4hc", "-C16384", "-T0", "-U", erofsUUID(value), stagedEROFS) //nolint:gosec
	var output bytes.Buffer
	cmd.Stderr = &output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create mkfs.erofs stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start mkfs.erofs: %w", err)
	}
	in, err := os.Open(layer.Path) //nolint:gosec
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, nil, nil, fmt.Errorf("open layer blob: %w", err)
	}
	reader, closeReader, err := layerTarReader(layer.MediaType, in)
	if err != nil {
		_ = in.Close()
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, nil, nil, err
	}
	kernel, initrd, scanErr := b.scanBootAndStream(reader, stdin, layer.Digest)
	closeReader()
	_ = in.Close()
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, nil, nil, scanErr
	}
	if closeErr != nil {
		return nil, nil, nil, fmt.Errorf("close mkfs.erofs input: %w", closeErr)
	}
	if waitErr != nil {
		return nil, nil, nil, fmt.Errorf("mkfs.erofs failed: %w: %s", waitErr, strings.TrimSpace(output.String()))
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("create EROFS blob dir: %w", err)
	}
	if err := os.Rename(stagedEROFS, target); err != nil && !os.IsExist(err) {
		return nil, nil, nil, fmt.Errorf("commit EROFS blob: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stat EROFS blob: %w", err)
	}
	sum, err := fileSHA256(target)
	if err != nil {
		return nil, nil, nil, err
	}
	return &imagestore.EROFSLayer{
		Path:        target,
		Filesystem:  "erofs",
		Digest:      "sha256:" + sum,
		SizeBytes:   info.Size(),
		SourceLayer: layer.Digest,
	}, kernel, initrd, nil
}

var erofsVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

func checkEROFSVersion(ctx context.Context, mkfs string) error {
	out, err := exec.CommandContext(ctx, mkfs, "--version").CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("EROFS_VERSION_UNAVAILABLE: %s: %w", strings.TrimSpace(string(out)), err)
	}
	match := erofsVersionPattern.FindStringSubmatch(string(out))
	if len(match) != 3 {
		return fmt.Errorf("EROFS_VERSION_INVALID: cannot parse mkfs.erofs version from %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < 1 || (major == 1 && minor < 8) {
		return fmt.Errorf("EROFS_VERSION_UNSUPPORTED: mkfs.erofs %s.%s requires at least 1.8", match[1], match[2])
	}
	return nil
}

func erofsUUID(value string) string {
	return fmt.Sprintf("%s-%s-5%s-8%s-%s", value[0:8], value[8:12], value[13:16], value[17:20], value[20:32])
}

// scanBootAndStream lets mkfs.erofs consume the same uncompressed tar stream
// that is inspected for boot assets. This avoids materializing a second tar.
func (b *Builder) scanBootAndStream(src io.Reader, dst io.Writer, sourceLayer string) (*bootAsset, *bootAsset, error) {
	tee := io.TeeReader(src, dst)
	tr := tar.NewReader(tee)
	var kernel, initrd *bootAsset
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read layer tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		kind, ok := bootAssetKind(hdr.Name)
		if !ok {
			continue
		}
		asset, err := b.commitBootAsset(kind, hdr.Name, sourceLayer, tr)
		if err != nil {
			return nil, nil, err
		}
		if kind == "kernel" && (kernel == nil || asset.SourcePath > kernel.SourcePath) {
			kernel = asset
		}
		if kind == "initrd" && (initrd == nil || asset.SourcePath > initrd.SourcePath) {
			initrd = asset
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, nil, fmt.Errorf("drain layer stream: %w", err)
	}
	return kernel, initrd, nil
}

func layerTarReader(mediaType string, src io.Reader) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(mediaType, ".tar+gzip"), strings.HasSuffix(mediaType, ".tar.gzip"):
		reader, err := gzip.NewReader(src)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open gzip layer: %w", err)
		}
		return reader, func() { _ = reader.Close() }, nil
	case strings.HasSuffix(mediaType, ".tar+zstd"), strings.HasSuffix(mediaType, ".tar.zstd"):
		reader, err := zstd.NewReader(src)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open zstd layer: %w", err)
		}
		return reader, reader.Close, nil
	case strings.HasSuffix(mediaType, ".tar"):
		return src, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("OCI_LAYER_MEDIA_TYPE_UNSUPPORTED: %s", mediaType)
	}
}

func splitDigest(digest string) (string, string, error) {
	algo, value, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(value) != sha256.Size*2 {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %s", digest)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %w", err)
	}
	return algo, value, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open file for sha256: %w", err)
	}
	defer file.Close() //nolint:errcheck

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func operationID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
