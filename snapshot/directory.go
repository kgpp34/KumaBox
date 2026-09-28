package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// directoryEnvelope lists the files of a published snapshot directory. Unlike
// tar, a directory transfer can reflink large files without reading them.
type directoryEnvelope struct {
	Version  int             `json:"version"`
	Snapshot types.Snapshot  `json:"snapshot"`
	Files    []directoryFile `json:"files"`
}

type directoryFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// WriteDirectory copies one immutable capture into an empty private directory
// and writes snapshot.json last. The caller publishes the directory atomically.
func WriteDirectory(ctx context.Context, source, destination string, record types.Snapshot) (returnErr error) {
	if err := record.Validate(); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if len(entries) == 0 || len(entries) > maxArchiveEntries {
		return errors.New("snapshot directory has an invalid file count")
	}
	envelope := directoryEnvelope{Version: archiveVersion, Snapshot: record}
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !validArchiveName(name) || name == archiveManifest || !entry.Type().IsRegular() {
			return fmt.Errorf("snapshot entry %q is not an allowed regular file", name)
		}
		from, to := filepath.Join(source, name), filepath.Join(destination, name)
		if err := storage.CloneFile(to, from); err != nil {
			return fmt.Errorf("copy snapshot entry %q: %w", name, err)
		}
		info, err := os.Lstat(to)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxArchiveBytes-total {
			return errors.Join(err, fmt.Errorf("snapshot entry %q has an invalid size", name))
		}
		total += info.Size()
		envelope.Files = append(envelope.Files, directoryFile{Name: name, Size: info.Size()})
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(encoded) > maxManifestBytes {
		return errors.New("snapshot directory manifest is too large")
	}
	file, err := os.OpenFile(filepath.Join(destination, archiveManifest), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // private caller-owned staging directory
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	return file.Sync()
}

// StageDirectory checks an exported directory and reflinks its files into a
// private destination. A caller may remove the destination after clone setup;
// the VMM's private hard links retain their own memory-file references.
func StageDirectory(ctx context.Context, source, destination string) (types.Snapshot, error) {
	manifestPath := filepath.Join(source, archiveManifest)
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxManifestBytes {
		return types.Snapshot{}, errors.Join(err, errors.New("snapshot directory manifest is missing or invalid"))
	}
	encoded, err := os.ReadFile(manifestPath) //nolint:gosec // verified regular bounded manifest
	if err != nil {
		return types.Snapshot{}, err
	}
	var envelope directoryEnvelope
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return types.Snapshot{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return types.Snapshot{}, errors.New("snapshot directory manifest has trailing data")
	}
	if envelope.Version != archiveVersion || len(envelope.Files) == 0 || len(envelope.Files) > maxArchiveEntries {
		return types.Snapshot{}, errors.New("snapshot directory manifest version or file count is invalid")
	}
	if err := envelope.Snapshot.Validate(); err != nil {
		return types.Snapshot{}, err
	}
	wanted := make(map[string]int64, len(envelope.Files))
	var total int64
	for _, file := range envelope.Files {
		if !validArchiveName(file.Name) || file.Name == archiveManifest || file.Size < 0 || file.Size > maxArchiveBytes-total {
			return types.Snapshot{}, fmt.Errorf("invalid snapshot directory entry %q", file.Name)
		}
		if _, exists := wanted[file.Name]; exists {
			return types.Snapshot{}, fmt.Errorf("duplicate snapshot directory entry %q", file.Name)
		}
		total += file.Size
		wanted[file.Name] = file.Size
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return types.Snapshot{}, err
	}
	if len(entries) != len(wanted)+1 {
		return types.Snapshot{}, errors.New("snapshot directory does not match its file list")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return types.Snapshot{}, err
		}
		if entry.Name() == archiveManifest {
			continue
		}
		size, expected := wanted[entry.Name()]
		if !expected || !entry.Type().IsRegular() {
			return types.Snapshot{}, fmt.Errorf("unexpected snapshot directory entry %q", entry.Name())
		}
		from, to := filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())
		if err := storage.CloneFile(to, from); err != nil {
			return types.Snapshot{}, err
		}
		copied, err := os.Lstat(to)
		if err != nil || !copied.Mode().IsRegular() || copied.Size() != size {
			return types.Snapshot{}, errors.Join(err, fmt.Errorf("snapshot entry %q changed during copy", entry.Name()))
		}
	}
	return envelope.Snapshot, nil
}
