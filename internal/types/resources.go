package types

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CPUPolicy is host scheduling policy, independent of the guest's vCPU count.
// Zero values derive a quota and weight from the vCPU count and permit one
// period of burst credit. A clone supplies its own policy; restore keeps the
// target policy. Snapshots never assign source-host CPUs to another machine.
type CPUPolicy struct {
	// Weight is the relative share under contention; zero uses the vCPU count.
	Weight uint32 `json:"weight,omitempty"`
	// QuotaUS is the CPU-time budget per period; zero uses vCPUs * PeriodUS.
	QuotaUS int64 `json:"quota_us,omitempty"`
	// PeriodUS is the bandwidth window; zero selects the kernel's 100ms default.
	PeriodUS int64 `json:"period_us,omitempty"`
	// BurstUS is saved burst credit: zero derives quota, -1 disables bursting.
	BurstUS int64 `json:"burst_us,omitempty"`
	// CPUSet is an explicit Linux CPU list, such as 0-3,6. Empty inherits placement.
	CPUSet string `json:"cpuset_cpus,omitempty"`
}

// Resolve derives defaults and validates kernel ranges without consulting a
// host. BurstUS remains -1 when disabled so resolving again is idempotent.
func (p CPUPolicy) Resolve(cpus uint32) (CPUPolicy, error) {
	if cpus == 0 {
		return CPUPolicy{}, errors.New("CPU count must be positive")
	}
	if p.Weight == 0 {
		p.Weight = min(cpus, 10_000)
	}
	if p.PeriodUS == 0 {
		p.PeriodUS = 100_000
	}
	if p.Weight > 10_000 || p.PeriodUS < 1_000 || p.PeriodUS > 1_000_000 {
		return CPUPolicy{}, errors.New("cpu-weight must be 1..10000 and cpu-period-us must be 1000..1000000 (or zero for defaults)")
	}
	if p.QuotaUS == 0 {
		p.QuotaUS = int64(cpus) * p.PeriodUS
	}
	if p.BurstUS == 0 {
		p.BurstUS = p.QuotaUS
	}
	if p.QuotaUS < 1_000 || p.BurstUS < -1 || p.BurstUS > p.QuotaUS {
		return CPUPolicy{}, errors.New("cpu-quota-us must be at least 1000; cpu-burst-us must be -1, zero for the default, or at most quota")
	}
	if _, err := cpuRanges(p.CPUSet); err != nil {
		return CPUPolicy{}, fmt.Errorf("cpuset-cpus: %w", err)
	}
	return p, nil
}

type cpuRange struct{ first, last uint64 }

// cpuRanges normalizes intervals rather than expanding CPU IDs. An invalid
// request such as 0-4294967295 must not allocate billions of entries.
func cpuRanges(value string) ([]cpuRange, error) {
	if value == "" {
		return nil, nil
	}
	var ranges []cpuRange
	for part := range strings.SplitSeq(value, ",") {
		lo, hi, interval := strings.Cut(part, "-")
		first, err := strconv.ParseUint(lo, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU list %q", value)
		}
		last := first
		if interval {
			last, err = strconv.ParseUint(hi, 10, 32)
			if err != nil || last < first {
				return nil, fmt.Errorf("invalid CPU list %q", value)
			}
		}
		ranges = append(ranges, cpuRange{first, last})
	}
	slices.SortFunc(ranges, func(a, b cpuRange) int {
		switch {
		case a.first < b.first:
			return -1
		case a.first > b.first:
			return 1
		default:
			return 0
		}
	})
	merged := ranges[:0]
	for _, next := range ranges {
		if len(merged) > 0 && next.first <= merged[len(merged)-1].last+1 {
			merged[len(merged)-1].last = max(merged[len(merged)-1].last, next.last)
		} else {
			merged = append(merged, next)
		}
	}
	return merged, nil
}

// CPUSetContains reports whether available grants every requested CPU. It
// compares ranges, so validation remains bounded even for a very large list.
func CPUSetContains(available, requested string) (bool, error) {
	granted, err := cpuRanges(available)
	if err != nil {
		return false, err
	}
	wanted, err := cpuRanges(requested)
	if err != nil {
		return false, err
	}
	index := 0
	for _, interval := range wanted {
		for index < len(granted) && granted[index].last < interval.first {
			index++
		}
		if index == len(granted) || granted[index].first > interval.first || granted[index].last < interval.last {
			return false, nil
		}
	}
	return true, nil
}
