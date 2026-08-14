// SPDX-License-Identifier: MIT

// Builder converts OCI layers into bootable KumaBox image data.
package oci

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

	"github.com/kumabox/kumabox/internal/agent/protocol"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/vm"
)

const ociCmdlineTemplate = "console=ttyS0 loglevel=3 clocksource=kvm-clock reboot=k panic=1 boot=kumabox-overlay kumabox.layers={{layers}} kumabox.cow={{cow}} kumabox.timeout=10 rw"

// BuildRequest describes an OCI image build.
type BuildRequest struct {
	Name         string
	Ref          string
	Platform     string
	Source       string
	MkfsEROFS    string
	Concurrency  int
	AgentProfile string
	Progress     func(ProgressEvent)
}

// Builder converts OCI layers into shared EROFS blobs and publishes an image record.
type Builder struct {
	rootDir  string
	erofsDir string
	stageDir string
	content  interface {
		Pull(context.Context, PullRequest) (*PullResult, error)
	}
	images interface {
		Create(image.CreateRequest) (*image.ImageRecord, error)
	}
}

// NewBuilder returns a Builder rooted under rootDir.
func NewBuilder(rootDir string) *Builder {
	return NewBuilderWithStores(rootDir, NewStore(rootDir), image.New(rootDir))
}

// NewBuilderWithStores creates a builder using caller-owned metadata stores.
func NewBuilderWithStores(rootDir string, content interface {
	Pull(context.Context, PullRequest) (*PullResult, error)
}, images interface {
	Create(image.CreateRequest) (*image.ImageRecord, error)
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
func (b *Builder) Build(ctx context.Context, req BuildRequest) (*image.ImageRecord, error) {
	if req.Name == "" {
		return nil, errors.New("image name must not be empty")
	}
	if req.Ref == "" {
		return nil, errors.New("OCI ref must not be empty")
	}
	if req.MkfsEROFS == "" {
		req.MkfsEROFS = "mkfs.erofs"
	}

	pull, err := b.content.Pull(ctx, PullRequest{
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
				layer: image.OCILayer{
					Index: i, Digest: layer.Digest, Serial: vm.LayerSerial(i),
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
				req.Progress(ProgressEvent{Phase: "erofs", Index: i, Total: len(pull.Layers), Digest: layer.Digest})
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	layers := make([]image.OCILayer, 0, len(results))
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
	boot := image.Boot{Mode: "direct", Kernel: kernel.Path, Initrd: initrd.Path, Cmdline: ociCmdlineTemplate}
	imageConfig, err := decodeOCIImageConfig(pull.Config.Path)
	if err != nil {
		return nil, err
	}
	agent, err := b.inspectAgentProfile(pull.Layers, req.AgentProfile)
	if err != nil {
		return nil, err
	}

	return b.images.Create(image.CreateRequest{
		Name: req.Name,
		Source: image.Source{
			Type: "oci",
			URI:  pull.DigestRef,
		},
		OS: image.OS{
			Family:  "linux",
			Profile: "oci-erofs",
		},
		Agent: agent,
		Boot:  boot,
		OCI: &image.OCI{
			Ref:       pull.Ref,
			Source:    pull.Source,
			DigestRef: pull.DigestRef,
			Platform: image.OCIPlatform{
				OS:           pull.Platform.OS,
				Architecture: pull.Platform.Architecture,
				Variant:      pull.Platform.Variant,
			},
			Config: image.OCIDescriptor{
				Digest:    pull.Config.Digest,
				MediaType: pull.Config.MediaType,
				SizeBytes: pull.Config.SizeBytes,
			},
			ImageConfig:    imageConfig,
			AgentInjection: agent.Injection,
			Layers:         layers,
			BuiltAt:        time.Now().UTC(),
		},
	})
}

type layerBuildResult struct {
	layer  image.OCILayer
	kernel *bootAsset
	initrd *bootAsset
}

func decodeOCIImageConfig(configPath string) (image.OCIImageConfig, error) {
	raw, err := os.ReadFile(configPath) //nolint:gosec
	if err != nil {
		return image.OCIImageConfig{}, fmt.Errorf("read OCI config: %w", err)
	}
	var doc struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return image.OCIImageConfig{}, fmt.Errorf("decode OCI config: %w", err)
	}
	if len(doc.Config) == 0 {
		return image.OCIImageConfig{}, nil
	}

	cfg := image.OCIImageConfig{}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Env"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.Env = value
	}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Cmd"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.Cmd = value
	}
	if value, ok, err := decodeConfigStringSlice(doc.Config, "Entrypoint"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.Entrypoint = value
	}
	if value, ok, err := decodeConfigString(doc.Config, "WorkingDir"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.Workdir = value
	}
	if value, ok, err := decodeConfigString(doc.Config, "User"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.User = value
	}
	if value, ok, err := decodeConfigLabels(doc.Config, "Labels"); err != nil {
		return image.OCIImageConfig{}, err
	} else if ok {
		cfg.Labels = value
	}
	return cfg, nil
}

func (b *Builder) inspectAgentProfile(layers []BlobRecord, mode string) (*image.AgentProfile, error) {
	if mode == "" {
		mode = image.AgentProfileAuto
	}
	if mode != image.AgentProfileAuto && mode != image.AgentProfileRequired && mode != image.AgentInjectionEmbedded && mode != image.AgentInjectionUnsupported {
		return nil, fmt.Errorf("AGENT_PROFILE_INVALID: %q", mode)
	}
	if mode == image.AgentInjectionUnsupported {
		return &image.AgentProfile{
			Name:      image.AgentName,
			Injection: image.AgentInjectionUnsupported,
		}, nil
	}

	var binaryFound, serviceFound bool
	for _, layer := range layers {
		in, err := os.Open(layer.Path) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("open agent profile layer: %w", err)
		}
		reader, closeReader, err := layerTarReader(layer.MediaType, in)
		if err != nil {
			_ = in.Close()
			return nil, err
		}
		tr := tar.NewReader(reader)
		for {
			hdr, nextErr := tr.Next()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				closeReader()
				_ = in.Close()
				return nil, fmt.Errorf("scan agent profile layer: %w", nextErr)
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			switch normalizeLayerPath(hdr.Name) {
			case strings.TrimPrefix(image.AgentBinaryPath, "/"):
				binaryFound = true
			case strings.TrimPrefix(image.AgentServicePath, "/"):
				serviceFound = true
			}
		}
		closeReader()
		if err := in.Close(); err != nil {
			return nil, fmt.Errorf("close agent profile layer: %w", err)
		}
	}

	if !binaryFound || !serviceFound {
		if mode == image.AgentProfileRequired || mode == image.AgentInjectionEmbedded {
			return nil, fmt.Errorf("AGENT_INJECTION_FAILED: image must contain %s and %s", image.AgentBinaryPath, image.AgentServicePath)
		}
		return &image.AgentProfile{
			Name:      image.AgentName,
			Injection: image.AgentInjectionUnsupported,
		}, nil
	}
	return &image.AgentProfile{
		Name:        image.AgentName,
		Injection:   image.AgentInjectionEmbedded,
		BinaryPath:  image.AgentBinaryPath,
		ServicePath: image.AgentServicePath,
		Capabilities: []string{
			string(protocol.CapabilityPingPong),
			string(protocol.CapabilityExec),
			string(protocol.CapabilityExecStream),
			string(protocol.CapabilityExecTTY),
			string(protocol.CapabilityIdentity),
			string(protocol.CapabilityReseed),
		},
	}, nil
}

func normalizeLayerPath(name string) string {
	return strings.TrimPrefix(path.Clean(strings.TrimPrefix(name, "/")), "./")
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

func (b *Builder) resolveBootProfile(layers []BlobRecord) (image.Boot, error) {
	var kernel *bootAsset
	var initrd *bootAsset

	for _, layer := range layers {
		layerKernel, layerInitrd, err := b.scanBootAssets(layer)
		if err != nil {
			return image.Boot{}, err
		}
		if layerKernel != nil {
			kernel = layerKernel
		}
		if layerInitrd != nil {
			initrd = layerInitrd
		}
	}
	if kernel == nil || initrd == nil {
		return image.Boot{}, fmt.Errorf("BOOT_PROFILE_UNSUPPORTED: OCI image must contain /boot/vmlinuz-* and /boot/initrd.img-*")
	}
	return image.Boot{
		Mode:    "direct",
		Kernel:  kernel.Path,
		Initrd:  initrd.Path,
		Cmdline: ociCmdlineTemplate,
	}, nil
}

func (b *Builder) scanBootAssets(layer BlobRecord) (kernel, initrd *bootAsset, err error) {
	in, err := os.Open(layer.Path) //nolint:gosec
	if err != nil {
		return nil, nil, fmt.Errorf("open layer blob: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, in, "close OCI layer blob")

	reader, closeReader, err := layerTarReader(layer.MediaType, in)
	if err != nil {
		return nil, nil, err
	}
	defer closeReader()

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

func (b *Builder) ensureEROFS(ctx context.Context, mkfs string, layer BlobRecord) (*image.EROFSLayer, error) {
	erofs, _, _, err := b.ensureEROFSWithAssets(ctx, mkfs, layer)
	return erofs, err
}

func (b *Builder) ensureEROFSWithAssets(ctx context.Context, mkfs string, layer BlobRecord) (*image.EROFSLayer, *bootAsset, *bootAsset, error) {
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

func (b *Builder) ensureEROFSOnce(ctx context.Context, mkfs string, layer BlobRecord) (*image.EROFSLayer, *bootAsset, *bootAsset, error) {
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
		return &image.EROFSLayer{
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
	return &image.EROFSLayer{
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

func operationID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
