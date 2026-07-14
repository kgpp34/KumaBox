package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// WriteNativeManifest validates the minimum Cloud Hypervisor payload and
// writes the publication manifest after the source VM has resumed.
func WriteNativeManifest(build *Build, rec *vmstore.VMRecord, disks []DiskManifest) (*Manifest, int64, error) {
	if build == nil || rec == nil {
		return nil, 0, errors.New("snapshot build and VM record are required")
	}
	pending := build.Record()
	nativeDir := filepath.Join(pending.StagingDir, "native")
	entries, err := os.ReadDir(nativeDir)
	if err != nil {
		return nil, 0, fmt.Errorf("read native snapshot payload: %w", err)
	}
	files := make([]NativeFileManifest, 0, len(entries))
	hasConfig := false
	hasState := false
	hasMemory := false
	var nativeSize int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, fmt.Errorf("stat native payload %s: %w", entry.Name(), err)
		}
		switch {
		case entry.Name() == "config.json":
			hasConfig = true
		case entry.Name() == "state.json":
			hasState = true
		case strings.HasPrefix(entry.Name(), "memory-range-"):
			hasMemory = true
		}
		files = append(files, NativeFileManifest{
			Path:      filepath.ToSlash(filepath.Join("native", entry.Name())),
			SizeBytes: info.Size(),
		})
		nativeSize += info.Size()
	}
	if !hasConfig || !hasState || !hasMemory {
		return nil, 0, fmt.Errorf("NATIVE_SNAPSHOT_INCOMPLETE: config=%t state=%t memory=%t", hasConfig, hasState, hasMemory)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	manifest := newDiskManifest(pending, rec, disks, writableDisks(rec))
	manifest.Type = "native"
	manifest.Consistency = "crash"
	manifest.Native = &NativeManifest{PayloadDir: "native", Files: files}
	manifest.CreatedAt = time.Now().UTC()
	if err := fileutil.WriteJSONAtomic(filepath.Join(pending.StagingDir, "snapshot.json"), manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, 0, fmt.Errorf("write native snapshot manifest: %w", err)
	}
	for _, disk := range disks {
		nativeSize += disk.AllocatedSizeBytes
	}
	return manifest, nativeSize, nil
}
