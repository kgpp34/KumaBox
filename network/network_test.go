package network

import (
	"testing"

	"github.com/kumabox/kumabox/types"
)

func TestQueueCountAndTAPName(t *testing.T) {
	if got := QueueCount(0); got != 2 {
		t.Fatalf("QueueCount(0) = %d, want 2", got)
	}
	if got := QueueCount(4); got != 8 {
		t.Fatalf("QueueCount(4) = %d, want 8", got)
	}
	id, err := types.ParseSandboxID("123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	name, err := TAPName("tap", id, 12)
	if err != nil {
		t.Fatal(err)
	}
	if name != "tap123e4567-12" || len(name) > linuxInterfaceNameLimit {
		t.Fatalf("TAPName = %q", name)
	}
}

func TestAddRangeRejectsInvalidBounds(t *testing.T) {
	if got := AddRange(-1, 1); got != nil {
		t.Fatalf("AddRange(-1, 1) = %#v", got)
	}
	got := AddRange(2, 2)
	if len(got) != 2 || got[0].Index != 2 || got[1].Index != 3 {
		t.Fatalf("AddRange(2, 2) = %#v", got)
	}
}
