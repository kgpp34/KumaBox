package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	paxSparseMap  = "KumaBox.sparse.map"
	paxSparseSize = "KumaBox.sparse.size"
)

// ExportOptions controls creation of a portable snapshot package.
type ExportOptions struct {
	Output      string
	Compression string
}

// Export writes a ready snapshot to a temporary file and atomically publishes it.
func (s *Store) Export(ctx context.Context, ref string, opts ExportOptions) error {
	if opts.Output == "" || !filepath.IsAbs(opts.Output) {
		return errors.New("snapshot export output must be an absolute path")
	}
	if opts.Compression == "" {
		opts.Compression = "none"
	}
	rec, lease, err := s.AcquireRead(ctx, ref)
	if err != nil {
		return err
	}
	defer lease.Release()                                                        //nolint:errcheck
	manifestRaw, err := os.ReadFile(filepath.Join(rec.DataDir, "snapshot.json")) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.ID != rec.ID || manifest.SchemaVersion != "kumabox.snapshot.v1" {
		return errors.New("SNAPSHOT_CORRUPT: manifest identity mismatch")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o755); err != nil {
		return fmt.Errorf("create export directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(opts.Output), ".kumabox-export-*.partial")
	if err != nil {
		return fmt.Errorf("create export temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	closer, writer, err := compressionWriter(tmp, opts.Compression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(writer)
	if err := writeTarBytes(tw, "manifest.json", manifestRaw); err != nil {
		return err
	}
	for _, disk := range manifest.Disks {
		if err := writeSparseDisk(ctx, tw, filepath.Join(rec.DataDir, filepath.FromSlash(disk.Path)), disk.Path); err != nil {
			return err
		}
	}
	checksums := strings.Builder{}
	for _, disk := range manifest.Disks {
		fmt.Fprintf(&checksums, "%s  %s\n", disk.SHA256, disk.Path)
	}
	if err := writeTarBytes(tw, "checksums.txt", []byte(checksums.String())); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close snapshot tar: %w", err)
	}
	if err := closer(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync snapshot export: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close snapshot export: %w", err)
	}
	if err := os.Rename(tmpPath, opts.Output); err != nil {
		return fmt.Errorf("publish snapshot export: %w", err)
	}
	ok = true
	return nil
}

func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func writeSparseDisk(ctx context.Context, tw *tar.Writer, path, name string) error {
	extents, logical, err := sparseExtents(path)
	if err != nil {
		return err
	}
	var physical int64
	parts := make([]string, 0, len(extents)*2)
	for _, extent := range extents {
		physical += extent.Length
		parts = append(parts, strconv.FormatInt(extent.Offset, 10), strconv.FormatInt(extent.Length, 10))
	}
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: physical, Typeflag: tar.TypeReg, Format: tar.FormatPAX, PAXRecords: map[string]string{paxSparseMap: strings.Join(parts, ","), paxSparseSize: strconv.FormatInt(logical, 10)}}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write sparse disk header: %w", err)
	}
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open snapshot disk: %w", err)
	}
	defer file.Close() //nolint:errcheck
	for _, extent := range extents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, io.NewSectionReader(file, extent.Offset, extent.Length), extent.Length); err != nil {
			return fmt.Errorf("stream sparse disk extent: %w", err)
		}
	}
	return nil
}

func compressionWriter(dst io.Writer, compression string) (func() error, io.Writer, error) {
	switch compression {
	case "none":
		return func() error { return nil }, dst, nil
	case "gzip":
		w := gzip.NewWriter(dst)
		return w.Close, w, nil
	case "zstd":
		w, err := zstd.NewWriter(dst)
		if err != nil {
			return nil, nil, fmt.Errorf("create zstd writer: %w", err)
		}
		return w.Close, w, nil
	default:
		return nil, nil, fmt.Errorf("unsupported compression %q", compression)
	}
}

type extent struct{ Offset, Length int64 }
