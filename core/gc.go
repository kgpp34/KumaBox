package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagescatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// GCAction identifies one lifecycle repair or orphan cleanup completed by GC.
type GCAction struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Action string `json:"action"`
}

// GCReport keeps completed work visible even when another independent cleanup
// fails. Busy resources are counted and retried by the next pass.
type GCReport struct {
	Actions []GCAction `json:"actions"`
	Skipped int        `json:"skipped"`
}

// ReconcileSandboxes exposes the same lightweight lifecycle pass used by the
// full collector without scanning artifact directories on every daemon tick.
func (s *SnapshotService) ReconcileSandboxes(ctx context.Context) ([]ReconcileAction, int, error) {
	if s == nil || s.lifecycle == nil {
		return nil, 0, errors.New("sandbox reconciliation service is not configured")
	}
	return s.lifecycle.ReconcileSandboxes(ctx)
}

// Collect repairs ownerless lifecycle states and reclaims only managed
// artifacts with no current catalog owner. It never evicts healthy snapshots.
//
//	discover -> lock each owner -> recheck catalog -> recover or delete
//	                           \ busy or changed owner -> next pass
func (s *SnapshotService) Collect(ctx context.Context) (report GCReport, returnErr error) {
	if s == nil || s.lifecycle == nil || s.store == nil || s.snapshots == nil {
		return report, errors.New("garbage collector is not configured")
	}
	report.Actions = make([]GCAction, 0)
	// Discover all modules before making a destructive decision. Each candidate
	// is rechecked again under its own lock immediately before collection.
	snapshotStates, err := snapshotcatalog.New(s.store).States(ctx)
	if err != nil {
		return report, err
	}
	sandboxIDs, err := s.discoverSandboxArtifacts(ctx)
	if err != nil {
		return report, err
	}
	snapshotIDs, restoreStages, err := s.discoverSnapshotArtifacts()
	if err != nil {
		return report, err
	}
	imagePaths, err := images.NewPaths(s.configuration.Paths)
	if err != nil {
		return report, err
	}
	imageCandidates, err := discoverImageArtifacts(imagePaths)
	if err != nil {
		return report, err
	}
	imageRecords, err := imagescatalog.New(s.store).List(ctx)
	if err != nil {
		return report, err
	}
	for _, image := range imageRecords {
		for _, layer := range image.Layers {
			delete(imageCandidates, layer.SourceDigest)
		}
	}
	for _, state := range snapshotStates {
		snapshotIDs[state.ID] = true
	}

	var failures []error
	actions, skipped, err := s.lifecycle.ReconcileSandboxes(ctx)
	report.Skipped += skipped
	if err != nil {
		failures = append(failures, err)
	}
	for _, action := range actions {
		report.Actions = append(report.Actions, GCAction{Kind: "sandbox", ID: action.ID.String(), Action: action.Action})
		if action.Action == "removed-stale-create" || action.Action == "finished-delete" {
			delete(sandboxIDs, action.ID)
		}
	}
	for _, id := range sortedSandboxIDs(sandboxIDs) {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		collected, busy, err := s.collectOrphanSandbox(ctx, id)
		if busy {
			report.Skipped++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("orphan sandbox %s: %w", id, err))
		} else if collected {
			report.Actions = append(report.Actions, GCAction{Kind: "sandbox", ID: id.String(), Action: "removed-orphan"})
		}
	}
	for _, id := range sortedSnapshotIDs(snapshotIDs) {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		action, busy, err := s.collectSnapshot(ctx, id, restoreStages[id])
		if busy {
			report.Skipped++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("snapshot %s: %w", id, err))
		} else if action != "" {
			report.Actions = append(report.Actions, GCAction{Kind: "snapshot", ID: id.String(), Action: action})
		}
	}
	for _, digest := range sortedImageDigests(imageCandidates) {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		collected, busy, err := s.collectOrphanImage(ctx, imagePaths, digest)
		if busy {
			report.Skipped++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("image layer %s: %w", digest, err))
		} else if collected {
			report.Actions = append(report.Actions, GCAction{Kind: "image-layer", ID: digest.String(), Action: "removed-orphan"})
		}
	}
	return report, errors.Join(failures...)
}

func (s *SnapshotService) discoverSandboxArtifacts(ctx context.Context) (map[types.SandboxID]bool, error) {
	paths, err := vmm.NewPaths(s.configuration.Paths)
	if err != nil {
		return nil, err
	}
	ids := make(map[types.SandboxID]bool)
	for _, directory := range []string{
		s.sandboxPaths.DataDir(), paths.RunBase(), paths.LogBase(),
		filepath.Join(s.configuration.Paths.Data, "vmm"),
	} {
		if err := scanIDs(directory, func(name string) bool {
			id, err := types.ParseSandboxID(name)
			if err == nil {
				ids[id] = true
			}
			return err == nil
		}); err != nil {
			return nil, err
		}
	}
	if err := scanIDs(s.configuration.VMM.CgroupParent, func(name string) bool {
		if !strings.HasPrefix(name, "sandbox-") || !strings.HasSuffix(name, ".scope") {
			return false
		}
		id, err := types.ParseSandboxID(strings.TrimSuffix(strings.TrimPrefix(name, "sandbox-"), ".scope"))
		if err == nil {
			ids[id] = true
		}
		return err == nil
	}); err != nil {
		return nil, err
	}
	for _, provider := range s.lifecycle.dependencies.networks.Providers() {
		collector, ok := provider.(network.GarbageCollector)
		if !ok {
			continue
		}
		owned, err := collector.OwnedIDs(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range owned {
			ids[id] = true
		}
	}
	return ids, nil
}

func (s *SnapshotService) discoverSnapshotArtifacts() (map[types.SnapshotID]bool, map[types.SnapshotID][]string, error) {
	ids := make(map[types.SnapshotID]bool)
	restoreStages := make(map[types.SnapshotID][]string)
	for _, directory := range []string{s.paths.DataDir(), s.paths.StagingDir()} {
		if err := scanIDs(directory, func(name string) bool {
			id, err := types.ParseSnapshotID(name)
			if err == nil {
				ids[id] = true
			}
			return err == nil
		}); err != nil {
			return nil, nil, err
		}
	}
	entries, err := os.ReadDir(s.paths.StagingDir())
	if errors.Is(err, fs.ErrNotExist) {
		return ids, restoreStages, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		id, valid := restoreStageSnapshotID(entry.Name())
		if !valid {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, nil, fmt.Errorf("snapshot restore stage %s is not a regular file", filepath.Join(s.paths.StagingDir(), entry.Name()))
		}
		ids[id] = true
		restoreStages[id] = append(restoreStages[id], entry.Name())
	}
	return ids, restoreStages, nil
}

func restoreStageSnapshotID(name string) (types.SnapshotID, bool) {
	prefix, suffix, found := strings.Cut(name, "-restore-")
	if !found || !strings.HasSuffix(suffix, ".raw") {
		return "", false
	}
	id, err := types.ParseSnapshotID(prefix)
	if err != nil {
		return "", false
	}
	if _, err := types.ParseSandboxID(strings.TrimSuffix(suffix, ".raw")); err != nil {
		return "", false
	}
	return id, true
}

// scanIDs ignores unrelated entries but rejects symlinks masquerading as a
// managed UUID directory. Ownership is checked again before deletion.
func scanIDs(directory string, accept func(string) bool) error {
	if err := storage.CheckPath(directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if accept(entry.Name()) && !entry.IsDir() {
			return fmt.Errorf("managed entry %s is not a directory", filepath.Join(directory, entry.Name()))
		}
	}
	return nil
}

func sortedSandboxIDs(ids map[types.SandboxID]bool) []types.SandboxID {
	ordered := make([]types.SandboxID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	return ordered
}

func sortedSnapshotIDs(ids map[types.SnapshotID]bool) []types.SnapshotID {
	ordered := make([]types.SnapshotID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	return ordered
}

func (s *SnapshotService) collectOrphanSandbox(ctx context.Context, id types.SandboxID) (collected, busy bool, returnErr error) {
	lockPath, err := s.sandboxPaths.Lock(id)
	if err != nil {
		return false, false, err
	}
	lock := filelock.New(lockPath)
	acquired, err := lock.TryLock(ctx)
	if err != nil || !acquired {
		return false, !acquired, err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	if _, err := s.sandboxes.Resolve(ctx, id.String()); err == nil {
		return false, false, nil
	} else if !isNotFound(err) {
		return false, false, err
	}
	paths, err := vmm.NewPaths(s.configuration.Paths)
	if err != nil {
		return false, false, err
	}
	process, err := paths.ReadProcess(id)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, false, err
	}
	if err == nil {
		for _, backend := range s.runtimes.Backends() {
			_, exists, locateErr := backend.Locate(ctx, id, process.Generation)
			if locateErr != nil {
				return false, false, locateErr
			}
			if exists {
				return false, false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("unowned VMM process for %s is still alive", id))
			}
		}
	}
	// An unowned VMM scope must be empty before cleanup; the backend refuses
	// to remove a live or unverified process, so dependent disks remain intact.
	for _, backend := range s.runtimes.Backends() {
		if err := backend.Cleanup(ctx, id); err != nil {
			return false, false, err
		}
	}
	for _, provider := range s.lifecycle.dependencies.networks.Providers() {
		if err := provider.Delete(ctx, id); err != nil {
			return false, false, err
		}
	}
	if err := s.lifecycle.dependencies.disks.Remove(ctx, id); err != nil {
		return false, false, err
	}
	for _, backend := range s.runtimes.Backends() {
		if err := backend.RemoveLogs(ctx, id); err != nil {
			return false, false, err
		}
	}
	return true, false, nil
}

func (s *SnapshotService) collectSnapshot(ctx context.Context, id types.SnapshotID, restoreStages []string) (action string, busy bool, returnErr error) {
	lockPath, err := s.paths.Lock(id)
	if err != nil {
		return "", false, err
	}
	lock := filelock.New(lockPath)
	acquired, err := lock.TryLock(ctx)
	if err != nil || !acquired {
		return "", !acquired, err
	}
	// A Deleting record can use the ordinary retryable Remove flow after the
	// recovery lock has been released. Other cases stay under this lock.
	remove := false
	defer func() {
		returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx)))
		if remove && returnErr == nil {
			_, returnErr = s.Remove(ctx, id.String())
		}
	}()
	catalog := snapshotcatalog.New(s.store)
	state, found, err := catalog.State(ctx, id)
	if err != nil {
		return "", false, err
	}
	removedRestoreStage, err := s.removeRestoreStages(id, restoreStages)
	if err != nil {
		return "", false, err
	}
	if found && state.Ready && !state.Deleting {
		published, err := s.paths.Dir(id)
		if err != nil {
			return "", false, err
		}
		if info, err := os.Lstat(published); errors.Is(err, fs.ErrNotExist) {
			remove = true
			return "removed-missing-dir", false, nil
		} else if err != nil {
			return "", false, err
		} else if !info.IsDir() {
			return "", false, fmt.Errorf("snapshot artifact %s is not a directory", published)
		}
		stage, err := s.paths.Stage(id)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Lstat(stage); errors.Is(err, fs.ErrNotExist) {
			if removedRestoreStage {
				return "removed-stale-restore", false, nil
			}
			return "", false, nil
		} else if err != nil {
			return "", false, err
		}
		if err := snapshot.IgnoreAbsence(s.paths.RemoveStage(id)); err != nil {
			return "", false, err
		}
		return "removed-stale-stage", false, nil
	}
	if found && state.Deleting {
		remove = true
		return "finished-delete", false, nil
	}
	if err := snapshot.IgnoreAbsence(s.paths.RemoveStage(id)); err != nil {
		return "", false, err
	}
	if err := snapshot.IgnoreAbsence(s.paths.Remove(id)); err != nil {
		return "", false, err
	}
	if found {
		if err := catalog.Forget(ctx, id); err != nil {
			return "", false, err
		}
		return "removed-stale-pending", false, nil
	}
	return "removed-orphan", false, nil
}

func (s *SnapshotService) removeRestoreStages(id types.SnapshotID, names []string) (bool, error) {
	directory := s.paths.StagingDir()
	removed := false
	for _, name := range names {
		owner, valid := restoreStageSnapshotID(name)
		if !valid || owner != id {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !info.Mode().IsRegular() {
			return removed, fmt.Errorf("snapshot restore stage %s is not a regular file", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

func isNotFound(err error) bool {
	code, ok := errdefs.CodeOf(err)
	return ok && code == errdefs.CodeNotFound
}
