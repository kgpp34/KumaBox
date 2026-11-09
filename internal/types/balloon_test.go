package types

import (
	"math"
	"testing"
)

func TestBalloonTargets(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"0%", 0},
		{"25%", 256 << 20},
		{"99%", 1063002112},
		{"256MiB", 256 << 20},
		{"8193", 8192},
		{" 1KiB ", 0},
	} {
		t.Run(test.input, func(t *testing.T) {
			target, err := ParseBalloonTarget(test.input)
			if err != nil {
				t.Fatal(err)
			}
			size, err := target.Resolve(1 << 30)
			if err != nil || size != test.want {
				t.Fatalf("target = %d, %v; want %d", size, err, test.want)
			}
		})
	}
	for _, input := range []string{"", "-1", "-1%", "100%", "1.5%", "%", "NaN", "9223372036854775808"} {
		if _, err := ParseBalloonTarget(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestBalloonTargetRejectsConflictsAndMemoryExhaustion(t *testing.T) {
	percent := uint32(25)
	invalid := uint32(100)
	for _, target := range []BalloonTarget{
		{Size: -1}, {Size: 1, Percent: &percent}, {Percent: &invalid}, {Size: 1 << 30},
	} {
		if _, err := target.Resolve(1 << 30); err == nil {
			t.Errorf("accepted %+v", target)
		}
	}
	if _, err := (BalloonTarget{}).Resolve(0); err == nil {
		t.Fatal("accepted zero guest memory")
	}
	percent = 99
	size, err := (BalloonTarget{Percent: &percent}).Resolve(math.MaxInt64)
	if err != nil || size <= math.MaxInt64/2 || size%BalloonPageSize != 0 {
		t.Fatalf("large guest memory target overflowed: %d, %v", size, err)
	}
	if got := DefaultBalloonSize(1<<30 + 8192); got != 256<<20 {
		t.Fatalf("default target is not page aligned: %d", got)
	}
}
