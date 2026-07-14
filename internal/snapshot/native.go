package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// WriteNativeManifest validates the minimum Cloud Hypervisor payload and
// writes the publication manifest after the source VM has resumed.
func WriteNativeManifest(ctx context.Context, build *Build, rec *vmstore.VMRecord, disks []DiskManifest, host backend.NativeHost) (*Manifest, int64, error) {
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
		digest, err := syncAndHashFile(ctx, filepath.Join(nativeDir, entry.Name()))
		if err != nil {
			return nil, 0, fmt.Errorf("checksum native payload %s: %w", entry.Name(), err)
		}
		files = append(files, NativeFileManifest{
			Path:      filepath.ToSlash(filepath.Join("native", entry.Name())),
			SizeBytes: info.Size(),
			SHA256:    digest,
		})
		nativeSize += info.Size()
	}
	if !hasConfig || !hasState || !hasMemory {
		return nil, 0, fmt.Errorf("NATIVE_SNAPSHOT_INCOMPLETE: config=%t state=%t memory=%t", hasConfig, hasState, hasMemory)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	manifest := newDiskManifest(pending, rec, disks, writableDisks(rec))
	manifest.SchemaVersion = "kumabox.snapshot.v2"
	manifest.Type = "native"
	manifest.Consistency = "crash"
	manifest.Native = &NativeManifest{PayloadDir: "native", Files: files}
	manifest.Backend = &BackendManifest{Name: host.BackendName, Version: host.BackendVersion, SnapshotFormat: host.SnapshotFormat}
	manifest.Machine = &MachineManifest{
		Architecture: host.Architecture, CPUVendor: host.CPUVendor,
		CPUFeatures: append([]string(nil), host.CPUFeatures...), VCPUs: rec.CPUs, MemoryBytes: rec.EffectiveMemoryBytes(),
	}
	boot, err := buildBootManifest(ctx, rec)
	if err != nil {
		return nil, 0, err
	}
	manifest.Boot = boot
	devices, err := buildNativeDeviceManifest(rec, nativeDir)
	if err != nil {
		return nil, 0, err
	}
	if devices.VCPUs != rec.CPUs || devices.MemoryBytes != rec.EffectiveMemoryBytes() {
		return nil, 0, errors.New("NATIVE_SNAPSHOT_INCOMPATIBLE: backend machine shape differs from VM record")
	}
	manifest.Devices = &devices.DeviceManifest
	manifest.Network = &NetworkManifest{RestorePolicy: "preserve", ClonePolicy: "new"}
	manifest.CreatedAt = time.Now().UTC()
	if err := fileutil.WriteJSONAtomic(filepath.Join(pending.StagingDir, "snapshot.json"), manifest, ".snapshot-manifest-*.tmp"); err != nil {
		return nil, 0, fmt.Errorf("write native snapshot manifest: %w", err)
	}
	if err := writeChecksums(pending.StagingDir, manifest); err != nil {
		return nil, 0, err
	}
	for _, disk := range disks {
		nativeSize += disk.AllocatedSizeBytes
	}
	return manifest, nativeSize, nil
}

func syncAndHashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hashFileContext(ctx, path)
}

func buildBootManifest(ctx context.Context, rec *vmstore.VMRecord) (*BootManifest, error) {
	boot := &BootManifest{Mode: "direct"}
	assets := []struct {
		path   string
		target *string
	}{
		{rec.Kernel, &boot.KernelDigest},
		{rec.Initrd, &boot.InitrdDigest},
		{rec.Firmware, &boot.FirmwareDigest},
	}
	if rec.Firmware != "" {
		boot.Mode = "uefi"
	}
	for _, asset := range assets {
		if asset.path == "" {
			continue
		}
		digest, err := hashFileContext(ctx, asset.path)
		if err != nil {
			return nil, fmt.Errorf("checksum boot asset %s: %w", asset.path, err)
		}
		*asset.target = "sha256:" + digest
	}
	return boot, nil
}

func writeChecksums(stagingDir string, manifest *Manifest) error {
	var lines strings.Builder
	for _, file := range manifest.Native.Files {
		fmt.Fprintf(&lines, "%s  %s\n", file.SHA256, file.Path)
	}
	for _, disk := range manifest.Disks {
		fmt.Fprintf(&lines, "%s  %s\n", disk.SHA256, disk.Path)
	}
	path := filepath.Join(stagingDir, "checksums.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create native checksums: %w", err)
	}
	if _, err := file.WriteString(lines.String()); err != nil {
		_ = file.Close()
		return fmt.Errorf("write native checksums: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync native checksums: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close native checksums: %w", err)
	}
	return nil
}
