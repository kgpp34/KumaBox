package gc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type Candidate struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type Report struct {
	DryRun     bool        `json:"dryRun"`
	CheckedAt  time.Time   `json:"checkedAt"`
	Candidates []Candidate `json:"candidates"`
}

func DryRun(cfg config.Config) (*Report, error) {
	records, err := vmstore.New(cfg.Runtime.RootDir).List()
	if err != nil {
		return nil, fmt.Errorf("read VM store: %w", err)
	}

	report := &Report{
		DryRun:     true,
		CheckedAt:  time.Now().UTC(),
		Candidates: []Candidate{},
	}
	liveRunDirs := map[string]struct{}{}
	liveLogDirs := map[string]struct{}{}

	for _, rec := range records {
		liveRunDirs[rec.RunDir] = struct{}{}
		liveLogDirs[rec.LogDir] = struct{}{}
		report.Candidates = append(report.Candidates, staleRuntimeFiles(rec)...)
	}

	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.RunDir, "vms"), liveRunDirs, "orphan_run_dir")...)
	report.Candidates = append(report.Candidates, orphanDirs(filepath.Join(cfg.Runtime.LogDir, "vms"), liveLogDirs, "orphan_log_dir")...)

	sort.Slice(report.Candidates, func(i, j int) bool {
		if report.Candidates[i].Path == report.Candidates[j].Path {
			return report.Candidates[i].Type < report.Candidates[j].Type
		}
		return report.Candidates[i].Path < report.Candidates[j].Path
	})
	return report, nil
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
				Path:   path,
				Type:   "stale_runtime_file",
				Reason: fmt.Sprintf("VM %s is %s but runtime file remains", rec.ID, rec.State),
			})
		}
	}
	return candidates
}

func orphanDirs(parent string, live map[string]struct{}, typ string) []Candidate {
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
			Path:   path,
			Type:   typ,
			Reason: "directory is not referenced by VM store",
		})
	}
	return candidates
}
