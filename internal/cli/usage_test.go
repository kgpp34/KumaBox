package cli

import (
	"testing"
	"time"
)

func TestParseUsageTime(t *testing.T) {
	parsed, err := parseUsageTime("since", "2026-08-12T10:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC)
	if parsed == nil || !parsed.Equal(want) {
		t.Fatalf("parsed = %v, want %s", parsed, want)
	}
	if _, err := parseUsageTime("until", "not-a-time"); err == nil {
		t.Fatal("invalid usage time was accepted")
	}
}
