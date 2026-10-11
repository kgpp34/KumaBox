package types

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ParseByteSize accepts positive integer bytes or binary size suffixes. Short
// KB/MB/GB/TB spellings are binary aliases so CLI resource flags agree.
func ParseByteSize(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("size must not be empty")
	}
	digits := 0
	for digits < len(value) && value[digits] >= '0' && value[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	number, err := strconv.ParseInt(value[:digits], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	multiplier, ok := map[string]int64{
		"": 1, "B": 1,
		"KB": 1 << 10, "KiB": 1 << 10,
		"MB": 1 << 20, "MiB": 1 << 20,
		"GB": 1 << 30, "GiB": 1 << 30,
		"TB": 1 << 40, "TiB": 1 << 40,
	}[value[digits:]]
	if !ok || number == 0 || number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid or overflowing size %q; use B, KiB, MiB, GiB, TiB, or their short binary aliases", value)
	}
	return number * multiplier, nil
}
