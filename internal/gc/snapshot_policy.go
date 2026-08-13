package gc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/resources"
	"github.com/kumabox/kumabox/internal/snapshot"
)

// SnapshotPolicy selects ready snapshots for deterministic LRU eviction.
type SnapshotPolicy struct {
	KeepLast    int           `json:"keepLast,omitempty"`
	KeepLastSet bool          `json:"-"`
	MaxAge      time.Duration `json:"maxAge,omitempty"`
	MaxBytes    int64         `json:"maxBytes,omitempty"`
}

// SnapshotPolicyCandidate explains one policy decision without relying on
// payload path naming conventions.
type SnapshotPolicyCandidate struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	SourceVMID     string             `json:"sourceVmId,omitempty"`
	Reason         string             `json:"reason"`
	SizeBytes      int64              `json:"sizeBytes"`
	LastAccessedAt time.Time          `json:"lastAccessedAt"`
	References     []reference.Record `json:"references,omitempty"`
}

// SnapshotPolicyReport is both the dry-run plan and the execution result.
type SnapshotPolicyReport struct {
	Policy             SnapshotPolicy            `json:"policy"`
	TotalBytes         int64                     `json:"totalBytes"`
	EstimatedFreeBytes int64                     `json:"estimatedFreeBytes"`
	EstimatedBytes     int64                     `json:"estimatedBytesAfter"`
	TargetSatisfied    bool                      `json:"targetSatisfied"`
	Candidates         []SnapshotPolicyCandidate `json:"candidates"`
	Blocked            []SnapshotPolicyCandidate `json:"blocked,omitempty"`
	Deleted            []SnapshotPolicyCandidate `json:"deleted,omitempty"`
	Skipped            []SnapshotPolicyCandidate `json:"skipped,omitempty"`
}

type snapshotPolicyItem struct {
	record     *snapshot.Record
	sourceVMID string
	references []reference.Record
	leased     bool
}

func (p SnapshotPolicy) validate() error {
	if p.KeepLast < 0 {
		return errors.New("snapshot keep count must not be negative")
	}
	if p.MaxAge < 0 {
		return errors.New("snapshot max age must not be negative")
	}
	if p.MaxBytes < 0 {
		return errors.New("snapshot max bytes must not be negative")
	}
	return nil
}

func planSnapshotPolicy(ctx context.Context, stores resources.StoreSet, policy SnapshotPolicy, now time.Time) (*SnapshotPolicyReport, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	records, err := stores.Snapshots.List()
	if err != nil {
		return nil, fmt.Errorf("list snapshots for policy GC: %w", err)
	}
	items := make([]snapshotPolicyItem, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		manifest, err := stores.Snapshots.PeekManifest(ctx, record.ID)
		if err != nil {
			return nil, fmt.Errorf("read snapshot %s owner for policy GC: %w", record.ID, err)
		}
		refs, err := stores.References.ListTarget(ctx, "snapshot", record.ID)
		if err != nil {
			return nil, fmt.Errorf("read snapshot %s references for policy GC: %w", record.ID, err)
		}
		leased, err := stores.Snapshots.IsLeased(record.ID)
		if err != nil {
			return nil, fmt.Errorf("read snapshot %s lease for policy GC: %w", record.ID, err)
		}
		items = append(items, snapshotPolicyItem{
			record: record, sourceVMID: manifest.Source.VMID,
			references: refs, leased: leased,
		})
	}
	return buildSnapshotPolicyPlan(items, policy, now), nil
}

func buildSnapshotPolicyPlan(items []snapshotPolicyItem, policy SnapshotPolicy, now time.Time) *SnapshotPolicyReport {
	report := &SnapshotPolicyReport{Policy: policy, TargetSatisfied: true}
	keepEnabled := policy.KeepLastSet || policy.KeepLast > 0
	groups := make(map[string][]snapshotPolicyItem)
	for _, item := range items {
		report.TotalBytes += snapshotAllocatedBytes(item.record)
		groups[item.sourceVMID] = append(groups[item.sourceVMID], item)
	}

	protected := make(map[string]struct{})
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			if group[i].record.CreatedAt.Equal(group[j].record.CreatedAt) {
				return group[i].record.ID > group[j].record.ID
			}
			return group[i].record.CreatedAt.After(group[j].record.CreatedAt)
		})
		if keepEnabled {
			limit := min(policy.KeepLast, len(group))
			for _, item := range group[:limit] {
				protected[item.record.ID] = struct{}{}
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		left, right := snapshotAccessTime(items[i].record), snapshotAccessTime(items[j].record)
		if left.Equal(right) {
			return items[i].record.ID < items[j].record.ID
		}
		return left.Before(right)
	})

	selected := make(map[string]*SnapshotPolicyCandidate)
	blocked := make(map[string]struct{})
	for _, item := range items {
		_, keep := protected[item.record.ID]
		var reasons []string
		if !keep && keepEnabled {
			reasons = append(reasons, "keep-last")
		}
		if !keep && policy.MaxAge > 0 && snapshotAccessTime(item.record).Before(now.Add(-policy.MaxAge)) {
			reasons = append(reasons, "max-age")
		}
		if len(reasons) == 0 {
			continue
		}
		candidate := newSnapshotPolicyCandidate(item, strings.Join(reasons, "+"))
		if blockedSnapshotPolicyItem(item) {
			report.Blocked = append(report.Blocked, candidate)
			blocked[item.record.ID] = struct{}{}
			continue
		}
		candidateCopy := candidate
		selected[item.record.ID] = &candidateCopy
	}

	projected := report.TotalBytes
	for _, candidate := range selected {
		projected -= candidate.SizeBytes
	}
	if policy.MaxBytes > 0 && projected > policy.MaxBytes {
		for _, item := range items {
			if projected <= policy.MaxBytes {
				break
			}
			if _, keep := protected[item.record.ID]; keep {
				continue
			}
			if candidate := selected[item.record.ID]; candidate != nil {
				candidate.Reason += "+max-bytes"
				continue
			}
			if _, isBlocked := blocked[item.record.ID]; isBlocked {
				continue
			}
			candidate := newSnapshotPolicyCandidate(item, "max-bytes")
			if blockedSnapshotPolicyItem(item) {
				report.Blocked = append(report.Blocked, candidate)
				continue
			}
			candidateCopy := candidate
			selected[item.record.ID] = &candidateCopy
			projected -= snapshotAllocatedBytes(item.record)
		}
	}

	for _, item := range items {
		if candidate := selected[item.record.ID]; candidate != nil {
			report.Candidates = append(report.Candidates, *candidate)
		}
	}
	report.EstimatedBytes = projected
	report.EstimatedFreeBytes = report.TotalBytes - projected
	if policy.MaxBytes > 0 && projected > policy.MaxBytes {
		report.TargetSatisfied = false
	}
	return report
}

func blockedSnapshotPolicyItem(item snapshotPolicyItem) bool {
	return item.sourceVMID == "" || item.leased || len(item.references) > 0
}

func newSnapshotPolicyCandidate(item snapshotPolicyItem, reason string) SnapshotPolicyCandidate {
	if item.sourceVMID == "" {
		reason += "+owner-unknown"
	}
	if item.leased {
		reason += "+leased"
	}
	if len(item.references) > 0 {
		reason += "+referenced"
	}
	return SnapshotPolicyCandidate{
		ID: item.record.ID, Name: item.record.Name, SourceVMID: item.sourceVMID,
		Reason: reason, SizeBytes: snapshotAllocatedBytes(item.record),
		LastAccessedAt: snapshotAccessTime(item.record), References: item.references,
	}
}

func snapshotAllocatedBytes(record *snapshot.Record) int64 {
	if record.AllocatedBytes > 0 {
		return record.AllocatedBytes
	}
	return record.SizeBytes
}

func snapshotAccessTime(record *snapshot.Record) time.Time {
	if !record.LastAccessedAt.IsZero() {
		return record.LastAccessedAt
	}
	return record.CreatedAt
}

func applySnapshotPolicy(ctx context.Context, stores resources.StoreSet, report *SnapshotPolicyReport) error {
	for _, candidate := range report.Candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := stores.Snapshots.Inspect(candidate.ID)
		if err != nil {
			report.Skipped = append(report.Skipped, candidate)
			continue
		}
		if !snapshotAccessTime(current).Equal(candidate.LastAccessedAt) {
			candidate.Reason += "+accessed-after-plan"
			report.Skipped = append(report.Skipped, candidate)
			continue
		}
		refs, err := stores.References.ListTarget(ctx, "snapshot", candidate.ID)
		if err != nil {
			return fmt.Errorf("recheck snapshot %s references: %w", candidate.ID, err)
		}
		if len(refs) > 0 {
			candidate.Reason += "+referenced-after-plan"
			candidate.References = refs
			report.Skipped = append(report.Skipped, candidate)
			continue
		}
		if err := fault.Check(ctx, fault.GCBeforeDelete); err != nil {
			return err
		}
		if _, err := stores.Snapshots.Remove(candidate.ID); err != nil {
			if errors.Is(err, snapshot.ErrInUse) || errors.Is(err, snapshot.ErrNotFound) {
				candidate.Reason += "+changed-after-plan"
				report.Skipped = append(report.Skipped, candidate)
				continue
			}
			return fmt.Errorf("evict snapshot %s: %w", candidate.ID, err)
		}
		if err := stores.References.DeleteSource(ctx, "snapshot", candidate.ID); err != nil {
			return fmt.Errorf("remove snapshot %s references: %w", candidate.ID, err)
		}
		report.Deleted = append(report.Deleted, candidate)
	}
	return nil
}
