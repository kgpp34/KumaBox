package snapshot

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	maxImportEntries   = 128
	maxImportFileSize  = int64(64 << 30)
	maxImportTotalSize = int64(128 << 30)
)

// ImportOptions controls secure package ingestion.
type ImportOptions struct {
	Input         string
	Name          string
	QEMUImgBinary string
}

// Import validates an untrusted package in staging before publishing it.
func (s *Store) Import(ctx context.Context, opts ImportOptions) (record *Record, err error) {
	if opts.Input == "" {
		return nil, errors.New("snapshot import input must not be empty")
	}
	build, err := s.Reserve(ctx, opts.Name)
	if err != nil {
		return nil, err
	}
	defer build.Abort()              //nolint:errcheck
	file, err := os.Open(opts.Input) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open snapshot package: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, file, "close snapshot package")
	reader, closeReader, err := compressionReader(file)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := closeReader(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	manifest, checksums, err := extractPackage(ctx, tar.NewReader(reader), build.Record().StagingDir)
	if err != nil {
		return nil, err
	}
	if err := validateImportedPayload(opts.QEMUImgBinary, build.Record().StagingDir, manifest, checksums); err != nil {
		return nil, err
	}
	manifest.ID = build.Record().ID
	manifest.Name = opts.Name
	if err := fileutil.WriteJSONAtomic(filepath.Join(build.Record().StagingDir, ManifestFile), manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, err
	}
	var size int64
	for _, disk := range manifest.Disks {
		size += disk.AllocatedSizeBytes
	}
	return build.Finalize(size)
}

func extractPackage(ctx context.Context, tr *tar.Reader, staging string) (*Manifest, map[string]string, error) {
	seen := make(map[string]struct{})
	var manifest *Manifest
	var checksums map[string]string
	var logicalTotal int64
	for count := 0; ; count++ {
		if count >= maxImportEntries {
			return nil, nil, errors.New("ARCHIVE_LIMIT_EXCEEDED: too many archive entries")
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read snapshot archive: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, nil, fmt.Errorf("ARCHIVE_UNSAFE: entry %q is not a regular file", hdr.Name)
		}
		name, err := safeArchivePath(hdr.Name)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, nil, fmt.Errorf("ARCHIVE_UNSAFE: duplicate entry %q", name)
		}
		seen[name] = struct{}{}
		if count == 0 && name != "manifest.json" {
			return nil, nil, errors.New("ARCHIVE_UNSAFE: manifest.json must be first")
		}
		switch {
		case name == "manifest.json":
			raw, err := readLimited(tr, hdr.Size, 4<<20)
			if err != nil {
				return nil, nil, err
			}
			var parsed Manifest
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return nil, nil, fmt.Errorf("SNAPSHOT_CORRUPT: decode manifest: %w", err)
			}
			if err := validateManifest(&parsed); err != nil {
				return nil, nil, err
			}
			manifest = &parsed
		case name == "checksums.txt":
			raw, err := readLimited(tr, hdr.Size, 4<<20)
			if err != nil {
				return nil, nil, err
			}
			checksums, err = parseChecksums(string(raw))
			if err != nil {
				return nil, nil, err
			}
		case strings.HasPrefix(name, DiskPathPrefix):
			if manifest == nil || !manifestDeclares(manifest, name) {
				return nil, nil, fmt.Errorf("SNAPSHOT_CORRUPT: undeclared payload %q", name)
			}
			logical, extents, err := parseSparseHeader(hdr)
			if err != nil {
				return nil, nil, err
			}
			logicalTotal += logical
			if logical > maxImportFileSize || logicalTotal > maxImportTotalSize {
				return nil, nil, errors.New("ARCHIVE_LIMIT_EXCEEDED: unpacked disk size exceeds limit")
			}
			if err := extractSparseFile(ctx, tr, filepath.Join(staging, filepath.FromSlash(name)), logical, extents); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf("ARCHIVE_UNSAFE: entry %q is not allowed", name)
		}
	}
	if manifest == nil || checksums == nil {
		return nil, nil, errors.New("SNAPSHOT_CORRUPT: manifest or checksums missing")
	}
	for _, disk := range manifest.Disks {
		if _, ok := seen[disk.Path]; !ok {
			return nil, nil, fmt.Errorf("SNAPSHOT_CORRUPT: payload %q missing", disk.Path)
		}
	}
	return manifest, checksums, nil
}

func parseSparseHeader(hdr *tar.Header) (int64, []extent, error) {
	logical, err := strconv.ParseInt(hdr.PAXRecords[paxSparseSize], 10, 64)
	if err != nil || logical < 0 {
		return 0, nil, errors.New("SNAPSHOT_CORRUPT: invalid sparse logical size")
	}
	if logical > maxImportFileSize {
		return 0, nil, errors.New("ARCHIVE_LIMIT_EXCEEDED: sparse file exceeds limit")
	}
	mapValue := hdr.PAXRecords[paxSparseMap]
	if mapValue == "" && hdr.Size == 0 {
		return logical, nil, nil
	}
	parts := strings.Split(mapValue, ",")
	if len(parts)%2 != 0 {
		return 0, nil, errors.New("SNAPSHOT_CORRUPT: invalid sparse map")
	}
	var extents []extent
	var physical, previousEnd int64
	for i := 0; i < len(parts); i += 2 {
		offset, e1 := strconv.ParseInt(parts[i], 10, 64)
		length, e2 := strconv.ParseInt(parts[i+1], 10, 64)
		if e1 != nil || e2 != nil || offset < previousEnd || length <= 0 || offset > logical || length > logical-offset {
			return 0, nil, errors.New("SNAPSHOT_CORRUPT: unsafe sparse extent")
		}
		extents = append(extents, extent{offset, length})
		previousEnd = offset + length
		physical += length
	}
	if physical != hdr.Size {
		return 0, nil, errors.New("SNAPSHOT_CORRUPT: sparse physical size mismatch")
	}
	return logical, extents, nil
}

func extractSparseFile(ctx context.Context, src io.Reader, path string, logical int64, extents []extent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create imported disk: %w", err)
	}
	ok := false
	defer func() {
		_ = dst.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := dst.Truncate(logical); err != nil {
		return err
	}
	for _, extent := range extents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := dst.Seek(extent.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, extent.Length); err != nil {
			return fmt.Errorf("extract sparse extent: %w", err)
		}
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	ok = true
	return dst.Close()
}

func validateImportedPayload(qemuBinary, staging string, manifest *Manifest, checksums map[string]string) error {
	if len(checksums) != len(manifest.Disks) {
		return errors.New("SNAPSHOT_CORRUPT: checksum set does not match declared payloads")
	}
	for _, disk := range manifest.Disks {
		path := filepath.Join(staging, filepath.FromSlash(disk.Path))
		digest, err := hashFile(path)
		if err != nil {
			return err
		}
		expected, ok := checksums[disk.Path]
		if !ok || expected != disk.SHA256 || digest != expected {
			return fmt.Errorf("CHECKSUM_MISMATCH: disk %s", disk.ID)
		}
		switch disk.Format {
		case vmstore.FormatQCOW2:
			info, err := storage.NewQEMUImg(qemuBinary).Info(context.Background(), path)
			if err != nil || info.Format != "qcow2" {
				return fmt.Errorf("SNAPSHOT_CORRUPT: disk %s is not qcow2", disk.ID)
			}
		case vmstore.FormatRaw:
			if disk.Filesystem == vmstore.FilesystemEXT4 {
				if err := validateExt4(path); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("SNAPSHOT_CORRUPT: unsupported disk format %q", disk.Format)
		}
	}
	return nil
}

func validateManifest(m *Manifest) error {
	if m.SchemaVersion != "kumabox.snapshot.v1" || m.Type != "disk" || m.Consistency != "stopped-disk" || len(m.Disks) == 0 {
		return errors.New("SNAPSHOT_CORRUPT: unsupported manifest")
	}
	seen := map[string]struct{}{}
	for _, d := range m.Disks {
		path, err := safeArchivePath(d.Path)
		if err != nil || !strings.HasPrefix(path, DiskPathPrefix) {
			return errors.New("SNAPSHOT_CORRUPT: invalid disk path")
		}
		if _, ok := seen[path]; ok {
			return errors.New("SNAPSHOT_CORRUPT: duplicate disk path")
		}
		seen[path] = struct{}{}
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if name == "" || filepath.IsAbs(name) || clean != name || clean == "." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("ARCHIVE_UNSAFE: unsafe path %q", name)
	}
	return clean, nil
}
func manifestDeclares(m *Manifest, path string) bool {
	for _, d := range m.Disks {
		if d.Path == path {
			return true
		}
	}
	return false
}
func readLimited(r io.Reader, size, limit int64) ([]byte, error) {
	if size < 0 || size > limit {
		return nil, errors.New("ARCHIVE_LIMIT_EXCEEDED: metadata entry too large")
	}
	return io.ReadAll(io.LimitReader(r, limit+1))
}
func parseChecksums(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, errors.New("SNAPSHOT_CORRUPT: invalid checksums file")
		}
		path, err := safeArchivePath(fields[1])
		if err != nil {
			return nil, err
		}
		if _, ok := out[path]; ok {
			return nil, errors.New("SNAPSHOT_CORRUPT: duplicate checksum")
		}
		out[path] = fields[0]
	}
	return out, nil
}
func hashFile(path string) (string, error) {
	return hashFileContext(context.Background(), path)
}

func hashFileContext(ctx context.Context, path string) (sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close snapshot file: %w", closeErr)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func validateExt4(path string) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close snapshot disk: %w", closeErr)
		}
	}()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1024+56); err != nil || magic[0] != 0x53 || magic[1] != 0xef {
		return errors.New("SNAPSHOT_CORRUPT: raw disk is not ext4")
	}
	return nil
}

func compressionReader(src io.Reader) (io.Reader, func() error, error) {
	buffered := bufio.NewReader(src)
	magic, _ := buffered.Peek(4)
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		r, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, nil, err
		}
		return r, r.Close, nil
	}
	if len(magic) == 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd {
		r, err := zstd.NewReader(buffered)
		if err != nil {
			return nil, nil, err
		}
		return r, func() error { r.Close(); return nil }, nil
	}
	return buffered, func() error { return nil }, nil
}
