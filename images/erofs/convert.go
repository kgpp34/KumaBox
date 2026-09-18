// Package erofs converts verified layer tar streams into deterministic EROFS
// artifacts and extracts regular boot candidates. It records overlay deletions
// for images.SelectBoot instead of choosing boot files within an individual layer.
package erofs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

const (
	// erofsBlockSize fixes compression cluster size for reproducible conversion.
	erofsBlockSize = 4096
)

// Converter implements images.Converter using mkfs.erofs with fixed output options.
// Its immutable configuration permits concurrent conversion into distinct work directories.
type Converter struct {
	// binary is the configured mkfs.erofs executable name or absolute path.
	binary string
	// architecture selects whether an extracted arm64 gzip kernel is decompressed.
	architecture string
	// limits bounds extracted boot files; source adapters bound the layer streams.
	limits images.Limits
}

var _ images.Converter = (*Converter)(nil)

// Options contains operator-controlled converter dependencies and bounds.
type Options struct {
	// Binary is the mkfs.erofs executable name or absolute path.
	Binary string
	// Limits bounds source streams and extracted boot files.
	Limits images.Limits
}

// New validates the target architecture and limits and requires mkfs.erofs >= 1.8.
// The target architecture can differ from the host running the conversion.
func New(ctx context.Context, architecture string, options Options) (*Converter, error) {
	if options.Binary == "" || !options.Limits.Valid() || (architecture != "amd64" && architecture != "arm64") {
		return nil, invalidLayer("invalid converter architecture or size limits")
	}
	output, err := exec.CommandContext(ctx, options.Binary, "--version").CombinedOutput() //nolint:gosec // the operator supplies a fixed executable; no shell is involved
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeHostIncompatible, fmt.Errorf("probe mkfs.erofs: %w (%s)", errors.Join(err, ctx.Err()), bytes.TrimSpace(output)))
	}
	if err := requireEROFSVersion(string(output)); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	return &Converter{binary: options.Binary, architecture: architecture, limits: options.Limits}, nil
}

// Convert streams a decompressed tar to mkfs.erofs while extracting boot files
// into workDir, which must already exist and belong to the caller. The caller
// owns staging cleanup and source closure. Fixed timestamps, compression options
// and a source-derived UUID make repeated conversion reproducible.
//
//	verified tar -> TeeReader -> boot scan -> staged kernel/initrd
//	                  |
//	                  v
//	             mkfs.erofs stdin -> staged EROFS -> hash and size
//
// Draining past tar EOF delivers the full stream to the child process and allows
// source verification to finish before the generated artifact is accepted.
func (c *Converter) Convert(ctx context.Context, descriptor types.Descriptor, source io.Reader, workDir string) (images.ConvertedLayer, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	outputPath := filepath.Join(workDir, descriptor.Digest.Hex()+".erofs")
	command := exec.CommandContext( //nolint:gosec // binary is fixed and every argument is derived from validated managed paths and digests
		ctx,
		c.binary,
		"--tar=f",
		"-zlz4hc",
		fmt.Sprintf("-C%d", erofsBlockSize),
		"-T0", // Stable filesystem timestamps are part of the converted artifact identity.
		"-U", deterministicUUID(descriptor.Digest),
		outputPath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		return images.ConvertedLayer{}, fmt.Errorf("open mkfs.erofs stdin: %w", err)
	}
	var commandOutput bytes.Buffer
	command.Stdout = &commandOutput
	command.Stderr = &commandOutput
	if err := command.Start(); err != nil {
		closeErr := stdin.Close()
		return images.ConvertedLayer{}, errors.Join(fmt.Errorf("start mkfs.erofs: %w", err), closeErr)
	}
	stream := io.TeeReader(source, stdin)
	bootFiles, whiteouts, opaque, scanErr := scanBoot(stream, workDir, c.architecture, c.limits.BootSize)
	if scanErr == nil {
		if _, err := io.Copy(io.Discard, stream); err != nil {
			scanErr = fmt.Errorf("drain layer tar: %w", err)
		}
	}
	if scanErr != nil {
		cancel()
	}
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("mkfs.erofs: %w (%s)", waitErr, strings.TrimSpace(commandOutput.String()))
	}
	if err := errors.Join(scanErr, closeErr, waitErr, ctx.Err()); err != nil {
		return images.ConvertedLayer{}, conversionError(err)
	}
	digest, size, err := digestPath(ctx, outputPath)
	if err != nil {
		return images.ConvertedLayer{}, err
	}
	return images.ConvertedLayer{
		SourceDigest: descriptor.Digest, EROFSPath: outputPath, EROFSDigest: digest,
		Size: size, BootFiles: bootFiles, Whiteouts: whiteouts, BootOpaque: opaque,
	}, nil
}

// scanBoot extracts only regular boot candidates and records lower-layer whiteouts.
// It never materializes arbitrary tar paths or follows archived links. Entries
// replacing /boot or a candidate with a non-regular node hide earlier candidates.
func scanBoot(source io.Reader, workDir, architecture string, limit int64) ([]images.StagedBootFile, []string, bool, error) {
	reader := tar.NewReader(source)
	var files []images.StagedBootFile
	var whiteouts []string
	opaque := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, whiteouts, opaque, nil
		}
		if err != nil {
			return nil, nil, false, invalidLayer("read layer tar: %v", err)
		}
		clean := filepath.ToSlash(filepath.Clean(header.Name))
		if filepath.IsAbs(header.Name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, nil, false, invalidLayer("unsafe layer path %q", header.Name)
		}
		if clean == ".wh.boot" || (clean == "boot" && header.Typeflag != tar.TypeDir) {
			files = nil
			opaque = true
			continue
		}
		if filepath.Dir(clean) != "boot" {
			continue
		}
		base := filepath.Base(clean)
		if base == ".wh..wh..opq" {
			opaque = true
			continue
		}
		if name, ok := strings.CutPrefix(base, ".wh."); ok {
			if images.IsBootName(name) {
				whiteouts = append(whiteouts, name)
			}
			continue
		}
		if !images.IsBootName(base) {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			whiteouts = append(whiteouts, base)
			// A later symlink or directory replaces a regular file from this layer too.
			files = slices.DeleteFunc(files, func(file images.StagedBootFile) bool { return file.Name == base })
			continue
		}
		destination := filepath.Join(workDir, base)
		if err := writeBootFile(reader, destination, strings.HasPrefix(base, "vmlinuz") && architecture == "arm64", limit); err != nil {
			return nil, nil, false, err
		}
		files = slices.DeleteFunc(files, func(file images.StagedBootFile) bool { return file.Name == base })
		files = append(files, images.StagedBootFile{Name: base, Path: destination})
	}
}

// writeBootFile bounds the final extracted size, including gzip expansion when
// arm64 kernel decompression is requested. Other boot files preserve source bytes.
func writeBootFile(source io.Reader, destination string, decompressKernel bool, limit int64) error {
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // destination is a managed staging path
	if err != nil {
		return fmt.Errorf("create boot artifact: %w", err)
	}
	input := source
	var gzipReader *gzip.Reader
	if decompressKernel {
		buffered := bufio.NewReader(source)
		input = buffered
		magic, peekErr := buffered.Peek(2)
		if peekErr != nil && !errors.Is(peekErr, io.EOF) {
			return errors.Join(peekErr, output.Close())
		}
		if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			gzipReader, err = gzip.NewReader(buffered)
			if err != nil {
				return errors.Join(fmt.Errorf("open compressed arm64 kernel: %w", err), output.Close())
			}
			input = gzipReader
		}
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, limit+1))
	var gzipErr error
	if gzipReader != nil {
		gzipErr = gzipReader.Close()
	}
	closeErr := output.Close()
	if err := errors.Join(copyErr, gzipErr, closeErr); err != nil {
		return fmt.Errorf("write boot artifact: %w", err)
	}
	if written == 0 || written > limit {
		return invalidLayer("boot artifact size %d is outside limit %d", written, limit)
	}
	return nil
}

// requireEROFSVersion accepts a major/minor version with tar-stream support.
// Version output may include the program name or a patch/suffix component.
func requireEROFSVersion(output string) error {
	fields := strings.FieldsFunc(output, func(char rune) bool {
		return (char < '0' || char > '9') && char != '.'
	})
	for _, field := range fields {
		parts := strings.Split(field, ".")
		if len(parts) < 2 {
			continue
		}
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		if majorErr != nil || minorErr != nil {
			continue
		}
		if major > 1 || (major == 1 && minor >= 8) {
			return nil
		}
		return fmt.Errorf("mkfs.erofs %d.%d is older than required 1.8", major, minor)
	}
	return fmt.Errorf("cannot parse mkfs.erofs version from %q", strings.TrimSpace(output))
}

// deterministicUUID derives stable UUID-shaped bytes from the source identity
// to avoid mkfs.erofs generating a different filesystem identity on each run.
func deterministicUUID(digest types.Digest) string {
	sum := sha256.Sum256([]byte(digest.String()))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// digestPath hashes the generated staged filesystem before it is published.
func digestPath(ctx context.Context, path string) (types.Digest, int64, error) {
	file, err := os.Open(path) //nolint:gosec // path is a managed staging path
	if err != nil {
		return types.Digest{}, 0, fmt.Errorf("open generated EROFS: %w", err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return types.Digest{}, 0, fmt.Errorf("hash generated EROFS: %w", err)
	}
	digest, err := types.ParseDigest(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
	return digest, size, err
}

func invalidLayer(format string, args ...any) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// conversionError preserves cancellation and classified errors while distinguishing
// compressed-data corruption from unavailable conversion artifacts.
func conversionError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, err)
	}
	if _, ok := errdefs.CodeOf(err); ok {
		return err
	}
	return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
}

// contextReader observes cancellation between reads of a generated local artifact.
type contextReader struct {
	// ctx stops further reads after cancellation.
	ctx context.Context
	// reader supplies the artifact bytes without taking ownership of its lifetime.
	reader io.Reader
}

// Read forwards data only while the context remains active.
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
