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
	"slices"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/types"
)

const (
	archiveVersion    = 1
	archiveManifest   = "snapshot.json"
	sparseMapKey      = "KUMABOX.sparse.map"
	sparseSizeKey     = "KUMABOX.sparse.size"
	maxArchiveEntries = 1024
	maxManifestBytes  = 1 << 20
	maxSparseMapBytes = 800 << 10
	maxArchiveBytes   = 16 << 40
)

// archiveExtent describes a data range in an otherwise sparse regular file.
type archiveExtent struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// archiveFile records the logical shape and checksum of one tar entry's bytes.
type archiveFile struct {
	Name       string          `json:"name"`
	Size       int64           `json:"size"`
	StoredSize int64           `json:"stored_size"`
	SHA256     string          `json:"sha256"`
	Extents    []archiveExtent `json:"extents,omitempty"`
}

// archiveEnvelope is the last tar entry. Its presence proves the file list was
// fully written; each entry is checked before an imported snapshot is published.
type archiveEnvelope struct {
	Version  int            `json:"version"`
	Snapshot types.Snapshot `json:"snapshot"`
	Files    []archiveFile  `json:"files"`
}

// WriteArchive streams immutable snapshot files into tar or gzip tar. Sparse
// disk extents are encoded in PAX records, so an empty logical disk need not
// consume its full logical size in the transport stream.
func WriteArchive(ctx context.Context, output io.Writer, directory string, record types.Snapshot, compress bool) (returnErr error) {
	if err := record.Validate(); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) == 0 || len(entries) > maxArchiveEntries {
		return errors.New("snapshot archive has an invalid file count")
	}
	var zipper *gzip.Writer
	if compress {
		zipper, err = gzip.NewWriterLevel(output, gzip.BestSpeed)
		if err != nil {
			return err
		}
		output = zipper
	}
	writer := tar.NewWriter(output)
	manifest := archiveEnvelope{Version: archiveVersion, Snapshot: record}
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !validArchiveName(name) || name == archiveManifest || !entry.Type().IsRegular() {
			return fmt.Errorf("snapshot entry %q is not an allowed regular file", name)
		}
		file, err := writeArchiveFile(ctx, writer, directory, name)
		if err != nil {
			return err
		}
		if file.Size > maxArchiveBytes-total {
			return errors.New("snapshot archive exceeds the size limit")
		}
		total += file.Size
		manifest.Files = append(manifest.Files, file)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(encoded) > maxManifestBytes {
		return errors.New("snapshot manifest is too large")
	}
	if err := writer.WriteHeader(&tar.Header{Name: archiveManifest, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(encoded))}); err != nil {
		return err
	}
	if _, err := writer.Write(encoded); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if zipper != nil {
		return zipper.Close()
	}
	return nil
}

func writeArchiveFile(ctx context.Context, writer *tar.Writer, directory, name string) (result archiveFile, returnErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return archiveFile{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	file, err := root.Open(name)
	if err != nil {
		return archiveFile{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return archiveFile{}, errors.Join(err, fmt.Errorf("snapshot entry %q is not a regular file", name))
	}
	if info.Size() < 0 || info.Size() > maxArchiveBytes {
		return archiveFile{}, fmt.Errorf("snapshot entry %q exceeds the size limit", name)
	}
	result = archiveFile{Name: name, Size: info.Size(), StoredSize: info.Size()}
	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: info.Size()}
	extents, sparse, err := scanSparse(file, info.Size())
	if err != nil {
		return archiveFile{}, err
	}
	if sparse {
		if extents == nil {
			extents = []archiveExtent{}
		}
		encoded, err := json.Marshal(extents)
		if err != nil {
			return archiveFile{}, err
		}
		if len(encoded) <= maxSparseMapBytes {
			result.Extents = extents
			result.StoredSize = 0
			for _, extent := range extents {
				result.StoredSize += extent.Length
			}
			header.Size = result.StoredSize
			header.PAXRecords = map[string]string{sparseMapKey: string(encoded), sparseSizeKey: strconv.FormatInt(info.Size(), 10)}
		}
	}
	if err := writer.WriteHeader(header); err != nil {
		return archiveFile{}, err
	}
	hash := sha256.New()
	destination := io.MultiWriter(writer, hash)
	if result.Extents != nil {
		for _, extent := range result.Extents {
			if _, err := file.Seek(extent.Offset, io.SeekStart); err != nil {
				return archiveFile{}, err
			}
			if _, err := io.CopyN(destination, contextReader{ctx, file}, extent.Length); err != nil {
				return archiveFile{}, err
			}
		}
	} else {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return archiveFile{}, err
		}
		if _, err := io.CopyN(destination, contextReader{ctx, file}, info.Size()); err != nil {
			return archiveFile{}, err
		}
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

// ReadArchive extracts a complete archive into an empty private staging
// directory. The caller owns cleanup, native VMM checks, and publication.
func ReadArchive(ctx context.Context, input io.Reader, directory string) (types.Snapshot, error) {
	buffered := bufio.NewReader(input)
	header, err := buffered.Peek(2)
	if err != nil {
		return types.Snapshot{}, fmt.Errorf("read snapshot archive header: %w", err)
	}
	var zipper *gzip.Reader
	var stream io.Reader = buffered
	if header[0] == 0x1f && header[1] == 0x8b {
		zipper, err = gzip.NewReader(buffered)
		if err != nil {
			return types.Snapshot{}, err
		}
		defer zipper.Close() //nolint:errcheck // read and checksum errors are returned below
		stream = zipper
	}
	reader := tar.NewReader(stream)
	actual := make(map[string]archiveFile)
	var manifest archiveEnvelope
	seenManifest := false
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return types.Snapshot{}, err
		}
		entry, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return types.Snapshot{}, fmt.Errorf("read snapshot tar: %w", err)
		}
		if seenManifest || entry.Typeflag != tar.TypeReg || !validArchiveName(entry.Name) {
			return types.Snapshot{}, fmt.Errorf("invalid snapshot tar entry %q", entry.Name)
		}
		if entry.Name == archiveManifest {
			if entry.Size <= 0 || entry.Size > maxManifestBytes {
				return types.Snapshot{}, errors.New("snapshot manifest size is invalid")
			}
			decoder := json.NewDecoder(io.LimitReader(reader, entry.Size))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return types.Snapshot{}, fmt.Errorf("decode snapshot manifest: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return types.Snapshot{}, errors.New("snapshot manifest contains trailing JSON")
			}
			seenManifest = true
			continue
		}
		if len(actual) >= maxArchiveEntries || actual[entry.Name].Name != "" {
			return types.Snapshot{}, fmt.Errorf("duplicate or excessive snapshot entry %q", entry.Name)
		}
		file, err := readArchiveFile(ctx, reader, directory, entry)
		if err != nil {
			return types.Snapshot{}, err
		}
		if file.Size > maxArchiveBytes-total {
			return types.Snapshot{}, errors.New("snapshot archive exceeds the size limit")
		}
		total += file.Size
		actual[entry.Name] = file
	}
	if !seenManifest || manifest.Version != archiveVersion || len(manifest.Files) != len(actual) || len(actual) == 0 {
		return types.Snapshot{}, errors.New("snapshot archive manifest or file list is incomplete")
	}
	if err := manifest.Snapshot.Validate(); err != nil {
		return types.Snapshot{}, fmt.Errorf("invalid snapshot metadata: %w", err)
	}
	seen := make(map[string]bool, len(manifest.Files))
	for _, expected := range manifest.Files {
		got, found := actual[expected.Name]
		if seen[expected.Name] || !found || got.Name != expected.Name || got.Size != expected.Size || got.StoredSize != expected.StoredSize || got.SHA256 != expected.SHA256 || !slices.Equal(got.Extents, expected.Extents) {
			return types.Snapshot{}, fmt.Errorf("snapshot entry %q does not match the manifest", expected.Name)
		}
		seen[expected.Name] = true
	}
	if zipper != nil {
		if _, err := io.Copy(io.Discard, contextReader{ctx, zipper}); err != nil {
			return types.Snapshot{}, fmt.Errorf("verify gzip trailer: %w", err)
		}
	}
	return manifest.Snapshot, nil
}

func readArchiveFile(ctx context.Context, reader *tar.Reader, directory string, entry *tar.Header) (result archiveFile, returnErr error) {
	result = archiveFile{Name: entry.Name, Size: entry.Size, StoredSize: entry.Size}
	if entry.Size < 0 || entry.Size > maxArchiveBytes {
		return archiveFile{}, fmt.Errorf("snapshot entry %q has an invalid size", entry.Name)
	}
	if encoded, ok := entry.PAXRecords[sparseMapKey]; ok {
		if len(encoded) > maxSparseMapBytes {
			return archiveFile{}, errors.New("snapshot sparse map is too large")
		}
		logical, err := strconv.ParseInt(entry.PAXRecords[sparseSizeKey], 10, 64)
		if err != nil || logical < 0 || logical > maxArchiveBytes {
			return archiveFile{}, fmt.Errorf("snapshot entry %q has an invalid sparse size", entry.Name)
		}
		if err := json.Unmarshal([]byte(encoded), &result.Extents); err != nil {
			return archiveFile{}, err
		}
		if result.Extents == nil {
			return archiveFile{}, errors.New("snapshot sparse map must be an array")
		}
		if err := validateExtents(result.Extents, logical, entry.Size); err != nil {
			return archiveFile{}, err
		}
		result.Size = logical
	} else if _, ok := entry.PAXRecords[sparseSizeKey]; ok {
		return archiveFile{}, errors.New("snapshot sparse size has no map")
	}
	file, err := os.OpenFile(filepath.Join(directory, entry.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // flat validated basename under private stage
	if err != nil {
		return archiveFile{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	if result.Extents != nil {
		if err := file.Truncate(result.Size); err != nil {
			return archiveFile{}, err
		}
	}
	hash := sha256.New()
	output := io.MultiWriter(file, hash)
	if result.Extents != nil {
		for _, extent := range result.Extents {
			if _, err := file.Seek(extent.Offset, io.SeekStart); err != nil {
				return archiveFile{}, err
			}
			if _, err := io.CopyN(output, contextReader{ctx, reader}, extent.Length); err != nil {
				return archiveFile{}, fmt.Errorf("extract sparse %s: %w", entry.Name, err)
			}
		}
	} else if _, err := io.CopyN(output, contextReader{ctx, reader}, entry.Size); err != nil {
		return archiveFile{}, fmt.Errorf("extract %s: %w", entry.Name, err)
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func validateExtents(extents []archiveExtent, logical, stored int64) error {
	var end, total int64
	for _, extent := range extents {
		if extent.Offset < end || extent.Length <= 0 || extent.Offset > logical || extent.Length > logical-extent.Offset || extent.Length > stored-total {
			return errors.New("snapshot sparse extents overlap or exceed their file")
		}
		end = extent.Offset + extent.Length
		total += extent.Length
	}
	if total != stored {
		return errors.New("snapshot sparse data size does not match its map")
	}
	return nil
}

func validArchiveName(name string) bool {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, ".") || name == ".." {
		return false
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '.', char == '_', char == '-':
		default:
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
