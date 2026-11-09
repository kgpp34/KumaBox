package types

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// BalloonPageSize is the virtio-balloon unit, independent of host page size.
const BalloonPageSize int64 = 4096

// BalloonTarget requests reclaimed guest memory in bytes or as a percentage.
// Zero bytes and zero percent both deflate the balloon completely.
type BalloonTarget struct {
	// Size is the byte target when Percent is nil.
	Size int64 `json:"size,omitempty"`
	// Percent selects an integer percentage of configured guest memory.
	Percent *uint32 `json:"percent,omitempty"`
}

// ParseBalloonTarget accepts integer percentages, zero, and resource byte sizes.
func ParseBalloonTarget(value string) (BalloonTarget, error) {
	value = strings.TrimSpace(value)
	if percent, ok := strings.CutSuffix(value, "%"); ok {
		n, err := strconv.ParseUint(percent, 10, 32)
		if err != nil || n > 99 {
			return BalloonTarget{}, errors.New("balloon percentage must be an integer from 0 to 99")
		}
		number := uint32(n)
		return BalloonTarget{Percent: &number}, nil
	}
	if value == "0" {
		return BalloonTarget{}, nil
	}
	size, err := ParseByteSize(value)
	if err != nil {
		return BalloonTarget{}, err
	}
	return BalloonTarget{Size: size}, nil
}

// Validate rejects conflicting units and invalid targets before host operations.
func (t BalloonTarget) Validate() error {
	invalidPercent := t.Percent != nil && (t.Size != 0 || *t.Percent > 99)
	if t.Size < 0 || invalidPercent {
		return errors.New("balloon target requires nonnegative bytes or a percentage from 0 to 99")
	}
	return nil
}

// Resolve rounds down to whole balloon pages, leaving some guest memory usable.
func (t BalloonTarget) Resolve(memory int64) (int64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	if memory <= 0 {
		return 0, errors.New("guest memory must be positive")
	}
	size := t.Size
	if t.Percent != nil {
		// Splitting the quotient avoids overflowing on large valid memories.
		percent := int64(*t.Percent)
		size = memory/100*percent + memory%100*percent/100
	}
	if size >= memory {
		return 0, fmt.Errorf("balloon target must be below guest memory %d", memory)
	}
	return size &^ (BalloonPageSize - 1), nil
}

// DefaultBalloonSize is the creation target for an enabled balloon device.
func DefaultBalloonSize(memory int64) int64 {
	return memory / 4 &^ (BalloonPageSize - 1)
}

// BalloonStatus reports live reclamation. Target acknowledgement precedes
// asynchronous guest reclaim; Actual is omitted when the VMM cannot report it.
type BalloonStatus struct {
	// Target is the requested number of reclaimed bytes.
	Target int64 `json:"target"`
	// Actual is the current number of reclaimed bytes, when available.
	Actual *int64 `json:"actual,omitempty"`
	// Default is the creation target applied to cold starts and new clones.
	Default int64 `json:"default"`
}
