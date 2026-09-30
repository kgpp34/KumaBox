package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	filelock "github.com/kumabox/kumabox/lock/flock"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/types"
)

// SnapshotEvictionPolicy adds explicit LRU eviction to the ordinary orphan GC.
// An enabled policy with no limits evicts every ready snapshot.
type SnapshotEvictionPolicy struct {
	// Enabled opts into evicting ready snapshots; zero limits select all.
	Enabled bool
	// DryRun previews eviction without deleting selected snapshots.
	DryRun bool
	// KeepLast retains this many most recently used snapshots.
	KeepLast int
	// MaxAge evicts snapshots not used within this duration.
	MaxAge time.Duration
	// MaxSize evicts least recently used snapshots until total size fits.
	MaxSize int64
}

func (p SnapshotEvictionPolicy) validate() error {
	if p.KeepLast < 0 || p.MaxAge < 0 || p.MaxSize < 0 {
		return errors.New("snapshot eviction limits must not be negative")
	}
	if !p.Enabled && (p.DryRun || p.KeepLast != 0 || p.MaxAge != 0 || p.MaxSize != 0) {
		return errors.New("snapshot eviction must be enabled before setting limits")
	}
	return nil
}

// CollectWithPolicy always runs the ordinary recovery pass first. LRU eviction
// is opt-in and never used by daemon's periodic orphan sweep.
func (s *MaintenanceService) CollectWithPolicy(ctx context.Context, policy SnapshotEvictionPolicy) (GCReport, error) {
	if err := policy.validate(); err != nil {
		return GCReport{}, err
	}
	report, err := s.Collect(ctx)
	if err != nil || !policy.Enabled {
		return report, err
	}
	records, err := snapshotcatalog.New(s.store).List(ctx)
	if err != nil {
		return report, err
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	candidates := pickSnapshotEvictions(records, policy, now)
	var failures []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		acted, busy, err := s.evictSnapshot(ctx, candidate.snapshot, policy.DryRun)
		if busy {
			report.Skipped++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("evict snapshot %s: %w", candidate.snapshot.ID, err))
		} else if acted {
			verb := "evicted:"
			if policy.DryRun {
				verb = "would-evict:"
			}
			report.Actions = append(report.Actions, GCAction{Kind: "snapshot", ID: candidate.snapshot.ID.String(), Action: verb + candidate.reason})
		}
	}
	return report, errors.Join(failures...)
}

type snapshotEviction struct {
	snapshot types.Snapshot
	reason   string
}

// pickSnapshotEvictions applies each limit independently. A snapshot survives
// only if it passes every requested limit; ties resolve by immutable ID.
func pickSnapshotEvictions(records []types.Snapshot, policy SnapshotEvictionPolicy, now time.Time) []snapshotEviction {
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(left, right types.Snapshot) int {
		if order := left.LastAccessedAt.Compare(right.LastAccessedAt); order != 0 {
			return order
		}
		return strings.Compare(left.ID.String(), right.ID.String())
	})
	reasons := make(map[types.SnapshotID][]string, len(sorted))
	if policy.KeepLast == 0 && policy.MaxAge == 0 && policy.MaxSize == 0 {
		for _, record := range sorted {
			reasons[record.ID] = []string{"lru-all"}
		}
	}
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, record := range sorted {
			if record.LastAccessedAt.Before(cutoff) {
				reasons[record.ID] = append(reasons[record.ID], "lru-age")
			}
		}
	}
	if policy.KeepLast > 0 && len(sorted) > policy.KeepLast {
		for _, record := range sorted[:len(sorted)-policy.KeepLast] {
			reasons[record.ID] = append(reasons[record.ID], "lru-keep")
		}
	}
	if policy.MaxSize > 0 {
		total := new(big.Int)
		for _, record := range sorted {
			total.Add(total, big.NewInt(record.Size))
		}
		limit := big.NewInt(policy.MaxSize)
		for _, record := range sorted {
			if total.Cmp(limit) <= 0 {
				break
			}
			reasons[record.ID] = append(reasons[record.ID], "lru-size")
			total.Sub(total, big.NewInt(record.Size))
		}
	}
	candidates := make([]snapshotEviction, 0, len(reasons))
	for _, record := range sorted {
		if labels := reasons[record.ID]; len(labels) > 0 {
			candidates = append(candidates, snapshotEviction{snapshot: record, reason: strings.Join(labels, "+")})
		}
	}
	return candidates
}

// evictSnapshot fences a selected access time under the snapshot lock before
// committing a deletion tombstone. A newer access causes a harmless skip.
func (s *MaintenanceService) evictSnapshot(ctx context.Context, selected types.Snapshot, dryRun bool) (acted, busy bool, returnErr error) {
	path, err := s.paths.Lock(selected.ID)
	if err != nil {
		return false, false, err
	}
	lock := filelock.New(path)
	acquired, err := lock.TryLock(ctx)
	if err != nil || !acquired {
		return false, !acquired, err
	}
	remove := false
	defer func() {
		returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx)))
		if remove && returnErr == nil {
			_, returnErr = s.snapshotService.Remove(ctx, selected.ID.String())
		}
	}()
	current, err := s.snapshots.Resolve(ctx, selected.ID.String())
	if isNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !current.LastAccessedAt.Equal(selected.LastAccessedAt) || current.Size != selected.Size {
		return false, false, nil
	}
	if dryRun {
		return true, false, nil
	}
	if _, err := s.snapshots.BeginDelete(ctx, selected.ID.String()); err != nil {
		return false, false, err
	}
	remove = true
	return true, false, nil
}
