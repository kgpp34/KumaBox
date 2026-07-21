package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type nativeConfig struct {
	CPUs struct {
		BootVCPUs int `json:"boot_vcpus"`
	} `json:"cpus"`
	Memory struct {
		Size int64 `json:"size"`
	} `json:"memory"`
	Disks []struct {
		Path      string `json:"path"`
		Readonly  bool   `json:"readonly"`
		ImageType string `json:"image_type"`
	} `json:"disks"`
	Nets  []json.RawMessage `json:"net"`
	Vsock json.RawMessage   `json:"vsock"`
}

type nativeDeviceManifest struct {
	DeviceManifest
	VCPUs       int
	MemoryBytes int64
}

type NativeVerifyTarget struct {
	VM   *vmstore.VMRecord
	Host backend.NativeHost
}

// VerifyNative validates payload integrity and restore compatibility without
// mutating the target VM or acquiring backend resources.
func (s *Store) VerifyNative(ctx context.Context, ref string, target NativeVerifyTarget) (*Manifest, error) {
	rec, lease, err := s.AcquireRead(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck
	return s.VerifyNativeRecord(ctx, rec, target)
}

// VerifyNativeRecord validates a record whose caller already holds a read
// lease. Restore uses this form to keep one lease across preflight, staging,
// destructive mutation, and backend resume.
func (s *Store) VerifyNativeRecord(ctx context.Context, rec *Record, target NativeVerifyTarget) (*Manifest, error) {
	if rec == nil {
		return nil, errors.New("SNAPSHOT_NOT_FOUND: snapshot record is required")
	}
	if target.VM == nil {
		return nil, errors.New("SNAPSHOT_INCOMPATIBLE: target VM is required")
	}
	manifest, err := s.VerifyNativePayloadRecord(ctx, rec, target.Host)
	if err != nil {
		return nil, err
	}
	if err := verifyNativeVM(ctx, manifest, target.VM); err != nil {
		return nil, err
	}
	return manifest, nil
}

// VerifyNativePayloadRecord validates immutable payload and host compatibility
// without requiring the source VM to still exist. Clone uses this before it
// allocates any new VM or provider resources.
func (s *Store) VerifyNativePayloadRecord(ctx context.Context, rec *Record, host backend.NativeHost) (*Manifest, error) {
	if rec == nil {
		return nil, errors.New("SNAPSHOT_NOT_FOUND: snapshot record is required")
	}
	manifest, err := loadNativeManifest(rec)
	if err != nil {
		return nil, err
	}
	if err := verifyNativeFiles(ctx, rec.DataDir, manifest); err != nil {
		return nil, err
	}
	if err := verifyNativeConfig(rec.DataDir, manifest); err != nil {
		return nil, err
	}
	if err := verifyNativeHost(manifest, NativeVerifyTarget{Host: host}); err != nil {
		return nil, err
	}
	return manifest, nil
}

// VerifyNativeCloneTarget checks the newly allocated clone shape while
// intentionally allowing new VM paths and network identities.
func VerifyNativeCloneTarget(ctx context.Context, manifest *Manifest, target *vmstore.VMRecord) error {
	if manifest == nil || target == nil {
		return errors.New("SNAPSHOT_INCOMPATIBLE: clone target is required")
	}
	if manifest.Machine.VCPUs != target.CPUs || manifest.Machine.MemoryBytes != target.EffectiveMemoryBytes() {
		return errors.New("SNAPSHOT_INCOMPATIBLE: clone vCPU or memory shape mismatch")
	}
	if !cloneDevicesMatch(manifest.Devices, target) {
		return errors.New("SNAPSHOT_INCOMPATIBLE: clone device topology mismatch")
	}
	return verifyNativeVMAssets(ctx, manifest, target)
}

func loadNativeManifest(rec *Record) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(rec.DataDir, ManifestFile)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("SNAPSHOT_CORRUPT: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("SNAPSHOT_CORRUPT: decode manifest: %w", err)
	}
	if manifest.SchemaVersion != NativeSchemaV2 || manifest.ID != rec.ID || manifest.Type != NativeType {
		return nil, errors.New("SNAPSHOT_INCOMPATIBLE: snapshot is not a native v2 snapshot")
	}
	if manifest.Consistency != "crash" {
		return nil, fmt.Errorf("SNAPSHOT_INCOMPATIBLE: native snapshot consistency %q is unsupported", manifest.Consistency)
	}
	if manifest.Native == nil || manifest.Backend == nil || manifest.Machine == nil || manifest.Boot == nil || manifest.Devices == nil {
		return nil, errors.New("SNAPSHOT_CORRUPT: native compatibility metadata is incomplete")
	}
	return &manifest, nil
}

func verifyNativeFiles(ctx context.Context, dataDir string, manifest *Manifest) error {
	declared := make(map[string]string, len(manifest.Native.Files)+len(manifest.Disks))
	for _, file := range manifest.Native.Files {
		if _, exists := declared[file.Path]; exists {
			return fmt.Errorf("SNAPSHOT_CORRUPT: duplicate payload %s", file.Path)
		}
		if err := verifyPayloadFile(ctx, dataDir, file.Path, file.SizeBytes, file.SHA256); err != nil {
			return err
		}
		declared[file.Path] = file.SHA256
	}
	for _, disk := range manifest.Disks {
		if _, exists := declared[disk.Path]; exists {
			return fmt.Errorf("SNAPSHOT_CORRUPT: duplicate payload %s", disk.Path)
		}
		if err := verifyPayloadFile(ctx, dataDir, disk.Path, disk.VirtualSizeBytes, disk.SHA256); err != nil {
			return err
		}
		declared[disk.Path] = disk.SHA256
	}
	if err := verifyPayloadInventory(dataDir, declared); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "checksums.txt")) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) && allDigestsEmpty(declared) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("SNAPSHOT_CORRUPT: read checksums: %w", err)
	}
	checksums, err := parseChecksums(string(raw))
	if err != nil {
		return err
	}
	if len(checksums) != len(declared) {
		return errors.New("SNAPSHOT_CORRUPT: checksum set does not match native payload inventory")
	}
	for path, digest := range declared {
		if checksums[path] != digest {
			return fmt.Errorf("CHECKSUM_MISMATCH: %s", path)
		}
	}
	return nil
}

func verifyPayloadInventory(dataDir string, declared map[string]string) error {
	seen := make(map[string]struct{}, len(declared))
	for _, dir := range []string{NativePayloadDir, DiskPayloadDir} {
		entries, err := os.ReadDir(filepath.Join(dataDir, dir))
		if err != nil {
			return fmt.Errorf("SNAPSHOT_CORRUPT: read payload directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			relative := filepath.ToSlash(filepath.Join(dir, entry.Name()))
			if !entry.Type().IsRegular() {
				return fmt.Errorf("SNAPSHOT_CORRUPT: payload %s is not a regular file", relative)
			}
			if _, ok := declared[relative]; !ok {
				return fmt.Errorf("SNAPSHOT_CORRUPT: undeclared payload %s", relative)
			}
			seen[relative] = struct{}{}
		}
	}
	if len(seen) != len(declared) {
		return errors.New("SNAPSHOT_CORRUPT: payload inventory is incomplete")
	}
	return nil
}

func verifyPayloadFile(ctx context.Context, dataDir, relative string, size int64, expected string) error {
	clean, err := safeArchivePath(relative)
	if err != nil || clean != relative {
		return fmt.Errorf("SNAPSHOT_CORRUPT: unsafe payload path %q", relative)
	}
	path := filepath.Join(dataDir, filepath.FromSlash(clean))
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("SNAPSHOT_CORRUPT: payload %s missing: %w", relative, err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return fmt.Errorf("SNAPSHOT_CORRUPT: payload %s shape mismatch", relative)
	}
	if expected == "" {
		return nil
	}
	digest, err := hashFileContext(ctx, path)
	if err != nil {
		return fmt.Errorf("SNAPSHOT_CORRUPT: checksum %s: %w", relative, err)
	}
	if digest != expected {
		return fmt.Errorf("CHECKSUM_MISMATCH: %s", relative)
	}
	return nil
}

func allDigestsEmpty(declared map[string]string) bool {
	for _, digest := range declared {
		if digest != "" {
			return false
		}
	}
	return true
}

func verifyNativeConfig(dataDir string, manifest *Manifest) error {
	cfg, err := readNativeConfig(filepath.Join(dataDir, NativePayloadDir, NativeConfigFile))
	if err != nil {
		return err
	}
	if len(cfg.Disks) != len(manifest.Devices.Disks) {
		return errors.New("SNAPSHOT_CORRUPT: native disk topology does not match manifest")
	}
	if cfg.CPUs.BootVCPUs != manifest.Machine.VCPUs || cfg.Memory.Size != manifest.Machine.MemoryBytes || len(cfg.Nets) != manifest.Devices.NICs || (len(cfg.Vsock) > 0) != manifest.Devices.Vsock {
		return errors.New("SNAPSHOT_CORRUPT: native machine topology does not match manifest")
	}
	for i, disk := range manifest.Devices.Disks {
		if cfg.Disks[i].Path != disk.Path || cfg.Disks[i].Readonly != disk.Readonly {
			return fmt.Errorf("SNAPSHOT_CORRUPT: native disk %d does not match manifest", i)
		}
	}
	return nil
}

func readNativeConfig(path string) (*nativeConfig, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("SNAPSHOT_CORRUPT: read native config: %w", err)
	}
	var cfg nativeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("SNAPSHOT_CORRUPT: decode native config: %w", err)
	}
	return &cfg, nil
}

func buildNativeDeviceManifest(rec *vmstore.VMRecord, nativeDir string) (*nativeDeviceManifest, error) {
	cfg, err := readNativeConfig(filepath.Join(nativeDir, NativeConfigFile))
	if err != nil {
		return nil, err
	}
	result := &nativeDeviceManifest{
		DeviceManifest: DeviceManifest{Disks: make([]StorageDeviceManifest, 0, len(cfg.Disks)), NICs: len(cfg.Nets), Vsock: len(cfg.Vsock) > 0},
		VCPUs:          cfg.CPUs.BootVCPUs, MemoryBytes: cfg.Memory.Size,
	}
	if result.NICs != len(rec.NetworkConfigs) || result.Vsock != (rec.VsockSocket != "") {
		return nil, errors.New("NATIVE_SNAPSHOT_INCOMPATIBLE: backend network or vsock topology differs from VM record")
	}
	matchedStorage := 0
	for _, nativeDisk := range cfg.Disks {
		matched := false
		for _, disk := range rec.StorageConfigs {
			if disk.Path != nativeDisk.Path {
				continue
			}
			if disk.Readonly != nativeDisk.Readonly {
				return nil, fmt.Errorf("NATIVE_SNAPSHOT_INCOMPATIBLE: disk %s readonly state differs from VM record", disk.ID)
			}
			result.Disks = append(result.Disks, StorageDeviceManifest{
				ID: disk.ID, Role: string(disk.EffectiveRole()), Path: disk.Path,
				Readonly: nativeDisk.Readonly, Format: disk.EffectiveFormat(),
			})
			matched = true
			matchedStorage++
			break
		}
		if !matched && rec.Metadata != nil && rec.Metadata.CidataDisk == nativeDisk.Path {
			if !nativeDisk.Readonly {
				return nil, errors.New("NATIVE_SNAPSHOT_INCOMPATIBLE: cidata disk is writable")
			}
			result.Disks = append(result.Disks, StorageDeviceManifest{
				ID: vmstore.StorageIDCidata, Role: string(vmstore.StorageRoleCidata), Path: nativeDisk.Path,
				Readonly: nativeDisk.Readonly, Format: vmstore.FormatRaw,
			})
			matched = true
		}
		if !matched {
			return nil, fmt.Errorf("NATIVE_SNAPSHOT_INCOMPATIBLE: unrecorded disk %s", nativeDisk.Path)
		}
	}
	if matchedStorage != len(rec.StorageConfigs) {
		return nil, errors.New("NATIVE_SNAPSHOT_INCOMPATIBLE: backend disk set differs from VM record")
	}
	return result, nil
}

func verifyNativeHost(manifest *Manifest, target NativeVerifyTarget) error {
	if manifest.Backend.Name != target.Host.BackendName || manifest.Backend.SnapshotFormat != target.Host.SnapshotFormat {
		return errors.New("SNAPSHOT_INCOMPATIBLE: backend or native snapshot format mismatch")
	}
	if manifest.Backend.Version != target.Host.BackendVersion {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: backend version %s requires %s", target.Host.BackendVersion, manifest.Backend.Version)
	}
	if manifest.Machine.Architecture != target.Host.Architecture {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: architecture %s requires %s", target.Host.Architecture, manifest.Machine.Architecture)
	}
	if manifest.Machine.CPUVendor != target.Host.CPUVendor {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: CPU vendor %s requires %s", target.Host.CPUVendor, manifest.Machine.CPUVendor)
	}
	for _, feature := range manifest.Machine.CPUFeatures {
		if !slices.Contains(target.Host.CPUFeatures, feature) {
			return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: CPU feature %s is unavailable", feature)
		}
	}
	return nil
}

func verifyNativeVM(ctx context.Context, manifest *Manifest, target *vmstore.VMRecord) error {
	if manifest.Source.VMID != target.ID {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot belongs to VM %s", manifest.Source.VMID)
	}
	if manifest.Machine.VCPUs != target.CPUs || manifest.Machine.MemoryBytes != target.EffectiveMemoryBytes() {
		return errors.New("SNAPSHOT_INCOMPATIBLE: vCPU or memory shape mismatch")
	}
	if !targetDevicesMatch(manifest.Devices, target) {
		return errors.New("SNAPSHOT_INCOMPATIBLE: device topology mismatch")
	}
	return verifyNativeVMAssets(ctx, manifest, target)
}

func verifyNativeVMAssets(ctx context.Context, manifest *Manifest, target *vmstore.VMRecord) error {
	boot, err := buildBootManifest(ctx, target, true)
	if err != nil {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: resolve boot assets: %w", err)
	}
	if manifest.Boot.KernelDigest != "" && manifest.Boot.KernelDigest != boot.KernelDigest {
		return errors.New("SNAPSHOT_INCOMPATIBLE: kernel asset digest mismatch")
	}
	if manifest.Boot.InitrdDigest != "" && manifest.Boot.InitrdDigest != boot.InitrdDigest {
		return errors.New("SNAPSHOT_INCOMPATIBLE: initrd asset digest mismatch")
	}
	if manifest.Boot.FirmwareDigest != "" && manifest.Boot.FirmwareDigest != boot.FirmwareDigest {
		return errors.New("SNAPSHOT_INCOMPATIBLE: firmware asset digest mismatch")
	}
	if manifest.Source.ImageID != "" {
		if target.Image == nil || target.Image.ID != manifest.Source.ImageID || target.Image.Digest != manifest.Source.ImageDigest {
			return errors.New("SNAPSHOT_INCOMPATIBLE: immutable image digest mismatch")
		}
	}
	if manifest.Base != nil {
		var targetBase *vmstore.StorageBase
		for _, disk := range target.StorageConfigs {
			if disk.EffectiveRole() == vmstore.StorageRoleCOW {
				targetBase = disk.Base
				break
			}
		}
		if targetBase == nil || targetBase.Family != manifest.Base.Family || targetBase.ImageID != manifest.Base.ImageID || targetBase.Digest != manifest.Base.Digest || targetBase.Format != manifest.Base.Format || !slices.Equal(targetBase.LayerDigests, manifest.Base.LayerDigests) {
			return errors.New("SNAPSHOT_INCOMPATIBLE: immutable base or layer digest mismatch")
		}
	}
	return nil
}

func cloneDevicesMatch(devices *DeviceManifest, target *vmstore.VMRecord) bool {
	if devices == nil || devices.NICs != len(target.NetworkConfigs) || devices.Vsock != (target.VsockSocket != "") {
		return false
	}
	storageByID := make(map[string]vmstore.StorageConfig, len(target.StorageConfigs))
	for _, disk := range target.StorageConfigs {
		storageByID[disk.ID] = disk
	}
	matched := 0
	for _, device := range devices.Disks {
		if device.Role == string(vmstore.StorageRoleCidata) {
			if target.Metadata == nil || target.Metadata.CidataDisk == "" || !device.Readonly {
				return false
			}
			continue
		}
		disk, ok := storageByID[device.ID]
		if !ok || string(disk.EffectiveRole()) != device.Role || disk.Readonly != device.Readonly || disk.EffectiveFormat() != device.Format {
			return false
		}
		matched++
	}
	return matched == len(target.StorageConfigs)
}

func targetDevicesMatch(devices *DeviceManifest, target *vmstore.VMRecord) bool {
	if devices.NICs != len(target.NetworkConfigs) || devices.Vsock != (target.VsockSocket != "") {
		return false
	}
	storageByPath := make(map[string]vmstore.StorageConfig, len(target.StorageConfigs))
	for _, disk := range target.StorageConfigs {
		storageByPath[disk.Path] = disk
	}
	matchedStorage := 0
	for _, device := range devices.Disks {
		if device.Role == string(vmstore.StorageRoleCidata) {
			if target.Metadata == nil || target.Metadata.CidataDisk != device.Path || !device.Readonly {
				return false
			}
			continue
		}
		disk, ok := storageByPath[device.Path]
		if !ok || disk.ID != device.ID || string(disk.EffectiveRole()) != device.Role || disk.Readonly != device.Readonly || disk.EffectiveFormat() != device.Format {
			return false
		}
		matchedStorage++
	}
	return matchedStorage == len(target.StorageConfigs)
}
