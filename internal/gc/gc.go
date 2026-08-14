// Package gc identifies and repairs KumaBox-managed resources that are no
// longer owned by a live VM.
package gc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/lock"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vm"
)

// Candidate describes one file or directory that GC would remove in a future
// non-dry-run mode.
type Candidate struct {
	Component string `json:"component"`
	Path      string `json:"path"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
}

// Report is the result of a GC scan.
type Report struct {
	DryRun         bool                  `json:"dryRun"`
	CheckedAt      time.Time             `json:"checkedAt"`
	Candidates     []Candidate           `json:"candidates"`
	Repaired       []Candidate           `json:"repaired,omitempty"`
	Skipped        []Candidate           `json:"skipped,omitempty"`
	SnapshotPolicy *SnapshotPolicyReport `json:"snapshotPolicy,omitempty"`
}

// Options enables optional policy-based collection in addition to orphan
// reconciliation.
type Options struct {
	SnapshotPolicy *SnapshotPolicy
}

// DryRun scans VM, runtime, log, and image state for orphaned managed files.
//
// It never removes data. The report is intended for operator review and for
// validating GC policy before destructive cleanup is implemented.
func DryRun(cfg config.Config) (*Report, error) {
	return DryRunContext(context.Background(), cfg, Options{})
}

// DryRunContext scans with optional policy rules without deleting state.
func DryRunContext(ctx context.Context, cfg config.Config, options Options) (report *Report, err error) {
	stores, err := state.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("open resource stores: %w", err)
	}
	if stores.Metadata != nil {
		defer func() {
			if closeErr := stores.Metadata.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close metadata store: %w", closeErr))
			}
		}()
	}
	return scan(ctx, cfg, stores, options)
}

func scan(ctx context.Context, cfg config.Config, stores state.Set, options Options) (*Report, error) {
	records, err := stores.VM.List()
	if err != nil {
		return nil, fmt.Errorf("read VM store: %w", err)
	}
	images, err := stores.Images.List()
	if err != nil {
		return nil, fmt.Errorf("read image store: %w", err)
	}
	networkStore := stores.Networks
	networkRecords, err := networkStore.List()
	if err != nil {
		return nil, fmt.Errorf("read network store: %w", err)
	}
	leases, err := networkStore.ListLeases()
	if err != nil {
		return nil, fmt.Errorf("read network leases: %w", err)
	}
	snapshotStore := stores.Snapshots
	snapshots, err := snapshotStore.Scan()
	if err != nil {
		return nil, fmt.Errorf("read snapshot store: %w", err)
	}

	report := &Report{
		DryRun:     true,
		CheckedAt:  time.Now().UTC(),
		Candidates: []Candidate{},
	}
	liveRunDirs := map[string]struct{}{}
	liveLogDirs := map[string]struct{}{}
	liveImageIDs := map[string]struct{}{}
	liveStorageDirs := map[string]struct{}{}
	liveOCIPaths := map[string]struct{}{}
	liveOCIDigests := map[string]struct{}{}

	for _, rec := range records {
		liveRunDirs[rec.RunDir] = struct{}{}
		liveLogDirs[rec.LogDir] = struct{}{}
		liveStorageDirs[filepath.Join(cfg.Runtime.RootDir, "storage", "vms", rec.ID)] = struct{}{}
		if rec.Image != nil && rec.Image.ID != "" {
			liveImageIDs[rec.Image.ID] = struct{}{}
		}
		addLivePath(liveOCIPaths, rec.Kernel)
		addLivePath(liveOCIPaths, rec.Initrd)
		for _, storage := range rec.StorageConfigs {
			addLivePath(liveOCIPaths, storage.Path)
		}
		report.Candidates = append(report.Candidates, staleRuntimeFiles(rec)...)
		report.Candidates = append(report.Candidates, staleRestoreStaging(rec, report.CheckedAt)...)
	}
	snapshotCandidates, err := snapshotGCCandidates(
		ctx,
		snapshotStore,
		cfg.Runtime.RootDir,
		snapshots,
		report.CheckedAt,
		liveImageIDs,
		liveOCIPaths,
		liveOCIDigests,
	)
	if err != nil {
		return nil, err
	}
	for _, image := range images {
		addLiveImageOCI(liveOCIPaths, liveOCIDigests, image)
	}

	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.RunDir, "vms"), liveRunDirs, "runtime", "orphan_run_dir")...)
	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.LogDir, "vms"), liveLogDirs, "runtime", "orphan_log_dir")...)
	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.RootDir, "storage", "vms"), liveStorageDirs, "storage", "orphan_vm_storage")...)
	report.Candidates = append(report.Candidates, snapshotCandidates...)
	report.Candidates = append(report.Candidates, imageCandidates(cfg.Runtime.RootDir, images, liveImageIDs)...)
	report.Candidates = append(report.Candidates, ociCandidates(cfg.Runtime.RootDir, liveOCIPaths, liveOCIDigests)...)
	report.Candidates = append(report.Candidates, networkCandidates(records, networkRecords, leases)...)
	if options.SnapshotPolicy != nil {
		policyReport, err := planSnapshotPolicy(ctx, stores, *options.SnapshotPolicy, report.CheckedAt)
		if err != nil {
			return nil, err
		}
		report.SnapshotPolicy = policyReport
	}

	sort.Slice(report.Candidates, func(i, j int) bool {
		if report.Candidates[i].Path == report.Candidates[j].Path {
			return report.Candidates[i].Type < report.Candidates[j].Type
		}
		return report.Candidates[i].Path < report.Candidates[j].Path
	})
	return report, nil
}

// Repair rescans before acting, then removes only candidates inside managed
// roots. Network records are cleaned only when their VM is gone; drift on a
// live VM is reported and left for explicit reconciliation.
func Repair(cfg config.Config) (*Report, error) {
	return RepairContext(context.Background(), cfg)
}

// RepairContext excludes concurrent resource publication for the complete
// scan-and-delete cycle. Candidates are discovered only after the exclusive
// lock is held, so a report produced before lock acquisition is never used.
func RepairContext(ctx context.Context, cfg config.Config) (*Report, error) {
	return RepairWithOptions(ctx, cfg, Options{})
}

// RepairWithOptions performs orphan repair and optional snapshot policy
// eviction under one maintenance lock and one consistent resource setup.
func RepairWithOptions(ctx context.Context, cfg config.Config, options Options) (report *Report, err error) {
	maintenance, err := lock.NewGuard(cfg.Runtime.RootDir).BeginMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := maintenance.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release GC maintenance lock: %w", releaseErr))
		}
	}()

	stores, err := state.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("open resource stores for repair: %w", err)
	}
	if stores.Metadata != nil {
		defer func() {
			if closeErr := stores.Metadata.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close metadata store: %w", closeErr))
			}
		}()
	}
	report, err = scan(ctx, cfg, stores, options)
	if err != nil {
		return nil, err
	}
	networkRecords, err := stores.Networks.List()
	if err != nil {
		return nil, fmt.Errorf("read network records for repair: %w", err)
	}
	report.DryRun = false
	for _, candidate := range report.Candidates {
		if candidate.Component == "network" {
			if err := repairNetworkCandidate(ctx, cfg, stores.Networks, networkRecords, candidate); err != nil {
				return nil, err
			}
			if candidate.Type == "network_drift" {
				report.Skipped = append(report.Skipped, candidate)
			} else {
				report.Repaired = append(report.Repaired, candidate)
			}
			continue
		}
		if !managedCandidatePath(cfg, candidate.Path) {
			report.Skipped = append(report.Skipped, candidate)
			continue
		}
		if err := os.RemoveAll(candidate.Path); err != nil {
			return nil, fmt.Errorf("repair %s: %w", candidate.Path, err)
		}
		report.Repaired = append(report.Repaired, candidate)
	}
	if report.SnapshotPolicy != nil {
		if err := applySnapshotPolicy(ctx, stores, report.SnapshotPolicy); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func managedCandidatePath(cfg config.Config, path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	for _, root := range []string{cfg.Runtime.RootDir, cfg.Runtime.RunDir, cfg.Runtime.LogDir} {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func repairNetworkCandidate(ctx context.Context, cfg config.Config, store state.NetworkState, records []kbnetwork.Record, candidate Candidate) error {
	if candidate.Type == "network_drift" {
		return nil
	}
	providerStore, ok := store.(*kbnetwork.Store)
	if !ok {
		return fmt.Errorf("network repair requires a concrete network store")
	}
	allocator := kbnetwork.NewAllocatorWithStore(providerStore, cfg.Network)
	if candidate.Type == "orphan_lease" {
		return allocator.ReleaseIP(candidate.Path)
	}
	for _, rec := range records {
		if rec.ID != candidate.Path && rec.TAP != candidate.Path {
			continue
		}
		switch rec.Provider {
		case kbnetwork.ProviderHostTap:
			if err := kbnetwork.DeleteHostTap(rec.TAP); err != nil {
				return fmt.Errorf("delete stale tap %s: %w", rec.TAP, err)
			}
		case kbnetwork.ProviderCNI:
			if err := kbnetwork.DeleteCNI(ctx, cfg.Runtime.RootDir, cfg.Network, kbnetwork.CNIDeleteRequest{VMID: rec.VMID, Network: rec.Network, IfName: rec.IfName, TAP: rec.TAP, NetNSPath: rec.NetnsPath}); err != nil {
				return fmt.Errorf("delete stale CNI network %s: %w", rec.ID, err)
			}
		}
		if err := allocator.ReleaseIP(firstString(rec.IPs)); err != nil {
			return err
		}
		return store.DeleteRecord(rec.ID)
	}
	return nil
}

func snapshotGCCandidates(
	ctx context.Context,
	store state.SnapshotState,
	rootDir string,
	records []*snapshot.Record,
	now time.Time,
	liveImageIDs map[string]struct{},
	liveOCIPaths map[string]struct{},
	liveOCIDigests map[string]struct{},
) ([]Candidate, error) {
	const pendingGrace = time.Hour
	snapshotDir := filepath.Join(rootDir, "snapshot")
	liveStaging := make(map[string]struct{}, len(records))
	indexedIDs := make(map[string]struct{}, len(records))
	var candidates []Candidate
	for _, rec := range records {
		indexedIDs[rec.ID] = struct{}{}
		if rec.StagingDir != "" {
			liveStaging[rec.StagingDir] = struct{}{}
		}
		switch rec.State {
		case snapshot.StateReady:
			if _, err := os.Stat(rec.DataDir); errors.Is(err, os.ErrNotExist) {
				candidates = append(candidates, Candidate{Component: "snapshot", Path: rec.DataDir, Type: "missing_snapshot_payload", Reason: "ready snapshot index record has no payload directory"})
				continue
			} else if err != nil {
				return nil, fmt.Errorf("stat ready snapshot %s: %w", rec.ID, err)
			}
			manifest, err := store.PeekManifest(ctx, rec.ID)
			if err != nil {
				return nil, fmt.Errorf("read ready snapshot %s: %w", rec.ID, err)
			}
			if err := addSnapshotLiveSet(rootDir, manifest, liveImageIDs, liveOCIPaths, liveOCIDigests); err != nil {
				return nil, fmt.Errorf("read ready snapshot %s references: %w", rec.ID, err)
			}
		case snapshot.StatePending:
			if now.Sub(rec.UpdatedAt) < pendingGrace {
				continue
			}
			leased, err := store.IsLeased(rec.ID)
			if err != nil {
				return nil, fmt.Errorf("inspect snapshot lease %s: %w", rec.ID, err)
			}
			if !leased && rec.StagingDir != "" {
				candidates = append(candidates, Candidate{Component: "snapshot", Path: rec.StagingDir, Type: "stale_pending_snapshot", Reason: "pending snapshot exceeded the one hour grace period"})
			}
		case snapshot.StateDeleting:
			if now.Sub(rec.UpdatedAt) >= pendingGrace {
				candidates = append(candidates, Candidate{Component: "snapshot", Path: rec.DataDir, Type: "stale_deleting_snapshot", Reason: "snapshot delete transaction exceeded the one hour grace period"})
			}
		}
	}
	entries, err := readDirIfExists(filepath.Join(snapshotDir, "staging"))
	if err != nil {
		return nil, fmt.Errorf("read snapshot staging directory: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(snapshotDir, "staging", entry.Name())
		if entry.IsDir() && pathOlderThan(path, now.Add(-pendingGrace)) {
			if _, ok := liveStaging[path]; !ok {
				candidates = append(candidates, Candidate{Component: "snapshot", Path: path, Type: "orphan_snapshot_staging", Reason: "staging directory has no snapshot index record"})
			}
		}
	}
	entries, err = readDirIfExists(snapshotDir)
	if err != nil {
		return nil, fmt.Errorf("read snapshot payload directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "snap_") {
			continue
		}
		path := filepath.Join(snapshotDir, entry.Name())
		if _, ok := indexedIDs[entry.Name()]; !ok {
			candidates = append(candidates, Candidate{Component: "snapshot", Path: path, Type: "orphan_snapshot_payload", Reason: "payload directory has no snapshot index record"})
		}
	}
	return candidates, nil
}

func addSnapshotLiveSet(rootDir string, manifest *snapshot.Manifest, imageIDs map[string]struct{}, paths map[string]struct{}, digests map[string]struct{}) error {
	if manifest == nil {
		return nil
	}
	if manifest.Source.ImageID != "" {
		imageIDs[manifest.Source.ImageID] = struct{}{}
	}
	if err := addSnapshotContentDigest(manifest.Source.ImageDigest, digests); err != nil {
		return fmt.Errorf("source image digest: %w", err)
	}
	if manifest.Base != nil {
		if manifest.Base.ImageID != "" {
			imageIDs[manifest.Base.ImageID] = struct{}{}
		}
		if err := addSnapshotContentDigest(manifest.Base.Digest, digests); err != nil {
			return fmt.Errorf("base digest: %w", err)
		}
		for _, digest := range manifest.Base.LayerDigests {
			if err := addSnapshotDigestAssets(rootDir, digest, paths, digests); err != nil {
				return fmt.Errorf("base layer digest: %w", err)
			}
		}
	}
	if manifest.Boot != nil {
		if err := addSnapshotBootAsset(rootDir, manifest.Boot.KernelDigest, paths); err != nil {
			return fmt.Errorf("kernel digest: %w", err)
		}
		if err := addSnapshotBootAsset(rootDir, manifest.Boot.InitrdDigest, paths); err != nil {
			return fmt.Errorf("initrd digest: %w", err)
		}
	}
	return nil
}

func addSnapshotDigestAssets(rootDir, digest string, paths map[string]struct{}, digests map[string]struct{}) error {
	algorithm, value, err := parseSnapshotDigest(digest)
	if err != nil || digest == "" {
		return err
	}
	digests[digest] = struct{}{}
	paths[filepath.Join(rootDir, "oci", "erofs", "blobs", algorithm, value+".erofs")] = struct{}{}
	return nil
}

func addSnapshotContentDigest(digest string, digests map[string]struct{}) error {
	_, _, err := parseSnapshotDigest(digest)
	if err != nil || digest == "" {
		return err
	}
	digests[digest] = struct{}{}
	return nil
}

func addSnapshotBootAsset(rootDir, digest string, paths map[string]struct{}) error {
	algorithm, value, err := parseSnapshotDigest(digest)
	if err != nil || digest == "" {
		return err
	}
	paths[filepath.Join(rootDir, "oci", "boot", "blobs", algorithm, value)] = struct{}{}
	return nil
}

func parseSnapshotDigest(digest string) (string, string, error) {
	if digest == "" {
		return "", "", nil
	}
	algorithm, value, ok := strings.Cut(digest, ":")
	if !ok || algorithm != "sha256" || len(value) != 64 {
		return "", "", fmt.Errorf("invalid digest %q", digest)
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", "", fmt.Errorf("invalid digest %q", digest)
		}
	}
	return algorithm, value, nil
}

func staleRestoreStaging(rec *vm.VMRecord, now time.Time) []Candidate {
	if rec == nil {
		return nil
	}
	path := filepath.Join(rec.RunDir, ".restore-staging")
	if !pathOlderThan(path, now.Add(-time.Hour)) {
		return nil
	}
	return []Candidate{{
		Component: "snapshot", Path: path, Type: "stale_restore_staging",
		Reason: "restore staging directory exceeded the one hour grace period",
	}}
}

func pathOlderThan(path string, cutoff time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && info.ModTime().Before(cutoff)
}

func readDirIfExists(path string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

func addLivePath(live map[string]struct{}, path string) {
	if path == "" {
		return
	}
	live[path] = struct{}{}
}

func addLiveImageOCI(paths map[string]struct{}, digests map[string]struct{}, image *image.ImageRecord) {
	if image == nil {
		return
	}
	addLivePath(paths, image.Boot.Kernel)
	addLivePath(paths, image.Boot.Initrd)
	if image.OCI == nil {
		return
	}
	if _, digest, ok := strings.Cut(image.OCI.DigestRef, "@"); ok {
		digests[digest] = struct{}{}
	}
	if image.OCI.Config.Digest != "" {
		digests[image.OCI.Config.Digest] = struct{}{}
	}
	for _, layer := range image.OCI.Layers {
		if layer.Digest != "" {
			digests[layer.Digest] = struct{}{}
		}
		if layer.EROFS != nil {
			addLivePath(paths, layer.EROFS.Path)
		}
		addLivePath(paths, layer.Kernel)
		addLivePath(paths, layer.Initrd)
	}
}

func networkCandidates(
	vms []*vm.VMRecord,
	records []kbnetwork.Record,
	leases map[string]kbnetwork.Lease,
) []Candidate {
	liveVMs := map[string]*vm.VMRecord{}
	vmConfigsByID := map[string]kbnetwork.Config{}
	liveIPs := map[string]struct{}{}
	for _, rec := range vms {
		if rec == nil {
			continue
		}
		liveVMs[rec.ID] = rec
		for _, cfg := range rec.NetworkConfigs {
			if cfg.ID != "" {
				vmConfigsByID[cfg.ID] = cfg
			}
			if cfg.Network != nil && cfg.Network.IP != "" {
				liveIPs[cfg.Network.IP] = struct{}{}
			}
		}
	}

	providerByID := map[string]kbnetwork.Record{}
	providerIPs := map[string]struct{}{}
	var candidates []Candidate
	for _, rec := range records {
		providerByID[rec.ID] = rec
		for _, ipCIDR := range rec.IPs {
			if ip := ipFromCIDR(ipCIDR); ip != "" {
				providerIPs[ip] = struct{}{}
			}
		}
		if rec.Cleanup.Pending {
			candidates = append(candidates, Candidate{
				Component: "network",
				Path:      rec.ID,
				Type:      "pending_cleanup",
				Reason:    rec.Cleanup.Reason,
			})
		}
		vmRec, vmExists := liveVMs[rec.VMID]
		vmCfg, cfgExists := vmConfigsByID[rec.ID]
		switch {
		case !vmExists:
			candidates = append(candidates, Candidate{
				Component: "network",
				Path:      rec.TAP,
				Type:      "stale_tap",
				Reason:    fmt.Sprintf("provider record %s references missing VM %s", rec.ID, rec.VMID),
			})
		case !cfgExists:
			candidates = append(candidates, Candidate{
				Component: "network",
				Path:      rec.ID,
				Type:      "network_drift",
				Reason:    fmt.Sprintf("provider record %s is missing from VM %s network configs", rec.ID, vmRec.ID),
			})
		case networkConfigDrift(vmCfg, rec):
			candidates = append(candidates, Candidate{
				Component: "network",
				Path:      rec.ID,
				Type:      "network_drift",
				Reason:    fmt.Sprintf("provider record %s differs from VM %s network config", rec.ID, vmRec.ID),
			})
		}
	}

	for cfgID := range vmConfigsByID {
		if _, ok := providerByID[cfgID]; ok {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: "network",
			Path:      cfgID,
			Type:      "network_drift",
			Reason:    "VM network config is missing provider record",
		})
	}

	for ip, lease := range leases {
		_, usedByVM := liveIPs[ip]
		_, usedByProvider := providerIPs[ip]
		if usedByVM || usedByProvider {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: "network",
			Path:      ip,
			Type:      "orphan_lease",
			Reason:    fmt.Sprintf("lease for tap %s is not referenced by VM or provider state", lease.TAP),
		})
	}
	return candidates
}

func networkConfigDrift(cfg kbnetwork.Config, rec kbnetwork.Record) bool {
	if cfg.TAP != rec.TAP || cfg.MAC != rec.MAC || cfg.Backend != rec.Provider || cfg.BridgeDev != rec.BridgeDev {
		return true
	}
	if cfg.Network == nil {
		return len(rec.IPs) > 0 || rec.Gateway != "" || len(rec.DNS) > 0
	}
	if cfg.Network.IP != ipFromCIDR(firstString(rec.IPs)) {
		return true
	}
	return cfg.Network.Gateway != rec.Gateway
}

func ipFromCIDR(value string) string {
	for i, r := range value {
		if r == '/' {
			return value[:i]
		}
	}
	return value
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func staleRuntimeFiles(rec *vm.VMRecord) []Candidate {
	if rec == nil || rec.State == vm.StateRunning {
		return nil
	}
	var candidates []Candidate
	for _, name := range []string{"ch.pid", "ch.sock", "vsock.uds"} {
		path := filepath.Join(rec.RunDir, name)
		if _, err := os.Stat(path); err == nil {
			typ := "stale_runtime_file"
			if name == "vsock.uds" {
				typ = "stale_agent_socket"
			}
			candidates = append(candidates, Candidate{
				Component: "runtime",
				Path:      path,
				Type:      typ,
				Reason:    fmt.Sprintf("VM %s is %s but runtime file remains", rec.ID, rec.State),
			})
		}
	}
	return candidates
}

func orphanDirs(parent string, live map[string]struct{}, component string, typ string) []Candidate {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var candidates []Candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if _, ok := live[path]; ok {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: component,
			Path:      path,
			Type:      typ,
			Reason:    "directory is not referenced by VM store",
		})
	}
	return candidates
}

func imageCandidates(rootDir string, images []*image.ImageRecord, liveImageIDs map[string]struct{}) []Candidate {
	cloudimgDir := filepath.Join(rootDir, "cloudimg")
	indexedIDs := make(map[string]struct{}, len(images))
	for _, image := range images {
		if image == nil {
			continue
		}
		indexedIDs[image.ID] = struct{}{}
	}

	var candidates []Candidate
	candidates = append(candidates, imageStagingCandidates(filepath.Join(cloudimgDir, "staging"))...)

	entries, err := os.ReadDir(cloudimgDir)
	if err != nil {
		return candidates
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "staging" {
			continue
		}
		if _, ok := indexedIDs[name]; ok {
			continue
		}
		if _, ok := liveImageIDs[name]; ok {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: "image",
			Path:      filepath.Join(cloudimgDir, name),
			Type:      "orphan_image_dir",
			Reason:    "image directory is not referenced by image index or VM store",
		})
	}
	return candidates
}

func imageStagingCandidates(stagingDir string) []Candidate {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return nil
	}
	var candidates []Candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: "image",
			Path:      filepath.Join(stagingDir, entry.Name()),
			Type:      "image_staging_dir",
			Reason:    "image staging directory is not referenced by image index",
		})
	}
	return candidates
}

func ociCandidates(rootDir string, livePaths map[string]struct{}, liveDigests map[string]struct{}) []Candidate {
	var candidates []Candidate
	candidates = append(candidates, ociStagingCandidates(filepath.Join(rootDir, "oci", "content", "staging"), "oci_content_staging")...)
	candidates = append(candidates, ociStagingCandidates(filepath.Join(rootDir, "oci", "staging"), "oci_build_staging")...)
	candidates = append(candidates, orphanOCIContentBlobs(rootDir, liveDigests)...)
	candidates = append(candidates, orphanOCIPathFiles(filepath.Join(rootDir, "oci", "erofs", "blobs"), livePaths, "oci", "orphan_erofs_blob")...)
	candidates = append(candidates, orphanOCIPathFiles(filepath.Join(rootDir, "oci", "boot", "blobs"), livePaths, "oci", "orphan_boot_asset")...)
	return candidates
}

func ociStagingCandidates(stagingDir string, typ string) []Candidate {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return nil
	}
	candidates := make([]Candidate, 0, len(entries))
	for _, entry := range entries {
		candidates = append(candidates, Candidate{
			Component: "oci",
			Path:      filepath.Join(stagingDir, entry.Name()),
			Type:      typ,
			Reason:    "OCI staging path is not referenced by committed image state",
		})
	}
	return candidates
}

func orphanOCIContentBlobs(rootDir string, liveDigests map[string]struct{}) []Candidate {
	blobsDir := filepath.Join(rootDir, "oci", "content", "blobs")
	files := listRegularFiles(blobsDir)
	var candidates []Candidate
	for _, file := range files {
		digest := digestFromBlobPath(blobsDir, file)
		if digest == "" {
			continue
		}
		if _, ok := liveDigests[digest]; ok {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: "oci",
			Path:      file,
			Type:      "orphan_content_blob",
			Reason:    fmt.Sprintf("OCI content blob %s is not referenced by any image", digest),
		})
	}
	return candidates
}

func orphanOCIPathFiles(root string, livePaths map[string]struct{}, component string, typ string) []Candidate {
	files := listRegularFiles(root)
	var candidates []Candidate
	for _, file := range files {
		if _, ok := livePaths[file]; ok {
			continue
		}
		candidates = append(candidates, Candidate{
			Component: component,
			Path:      file,
			Type:      typ,
			Reason:    "OCI artifact is not referenced by any image or VM",
		})
	}
	return candidates
}

func listRegularFiles(root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files
}

func digestFromBlobPath(root string, file string) string {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return ""
	}
	algo, value := filepath.Split(filepath.ToSlash(rel))
	algo = strings.TrimSuffix(algo, "/")
	if algo == "" || value == "" {
		return ""
	}
	return algo + ":" + value
}
