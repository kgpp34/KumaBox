// Package gc identifies KumaBox-managed files that are safe candidates for
// cleanup.
//
// The current phase is dry-run only. It reports stale runtime files and orphaned
// managed directories without deleting anything.
package gc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
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
	DryRun     bool        `json:"dryRun"`
	CheckedAt  time.Time   `json:"checkedAt"`
	Candidates []Candidate `json:"candidates"`
}

// DryRun scans VM, runtime, log, and image state for orphaned managed files.
//
// It never removes data. The report is intended for operator review and for
// validating GC policy before destructive cleanup is implemented.
func DryRun(cfg config.Config) (*Report, error) {
	records, err := vmstore.New(cfg.Runtime.RootDir).List()
	if err != nil {
		return nil, fmt.Errorf("read VM store: %w", err)
	}
	images, err := imagestore.New(cfg.Runtime.RootDir).List()
	if err != nil {
		return nil, fmt.Errorf("read image store: %w", err)
	}
	networkStore := kbnetwork.NewStore(cfg.Runtime.RootDir)
	networkRecords, err := networkStore.List()
	if err != nil {
		return nil, fmt.Errorf("read network store: %w", err)
	}
	leases, err := networkStore.ListLeases()
	if err != nil {
		return nil, fmt.Errorf("read network leases: %w", err)
	}

	report := &Report{
		DryRun:     true,
		CheckedAt:  time.Now().UTC(),
		Candidates: []Candidate{},
	}
	liveRunDirs := map[string]struct{}{}
	liveLogDirs := map[string]struct{}{}
	liveImageIDs := map[string]struct{}{}

	for _, rec := range records {
		liveRunDirs[rec.RunDir] = struct{}{}
		liveLogDirs[rec.LogDir] = struct{}{}
		if rec.Image != nil && rec.Image.ID != "" {
			liveImageIDs[rec.Image.ID] = struct{}{}
		}
		report.Candidates = append(report.Candidates, staleRuntimeFiles(rec)...)
	}

	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.RunDir, "vms"), liveRunDirs, "runtime", "orphan_run_dir")...)
	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.LogDir, "vms"), liveLogDirs, "runtime", "orphan_log_dir")...)
	report.Candidates = append(report.Candidates, imageCandidates(cfg.Runtime.RootDir, images, liveImageIDs)...)
	report.Candidates = append(report.Candidates, networkCandidates(records, networkRecords, leases)...)

	sort.Slice(report.Candidates, func(i, j int) bool {
		if report.Candidates[i].Path == report.Candidates[j].Path {
			return report.Candidates[i].Type < report.Candidates[j].Type
		}
		return report.Candidates[i].Path < report.Candidates[j].Path
	})
	return report, nil
}

func networkCandidates(
	vms []*vmstore.VMRecord,
	records []kbnetwork.Record,
	leases map[string]kbnetwork.Lease,
) []Candidate {
	liveVMs := map[string]*vmstore.VMRecord{}
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

func staleRuntimeFiles(rec *vmstore.VMRecord) []Candidate {
	if rec == nil || rec.State == vmstore.StateRunning {
		return nil
	}
	var candidates []Candidate
	for _, name := range []string{"ch.pid", "ch.sock"} {
		path := filepath.Join(rec.RunDir, name)
		if _, err := os.Stat(path); err == nil {
			candidates = append(candidates, Candidate{
				Component: "runtime",
				Path:      path,
				Type:      "stale_runtime_file",
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

func imageCandidates(rootDir string, images []*imagestore.ImageRecord, liveImageIDs map[string]struct{}) []Candidate {
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
