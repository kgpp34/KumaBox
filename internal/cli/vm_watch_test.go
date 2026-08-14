package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vm"
)

func TestWriteVMEventTable(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	events := []kbruntime.VMStatusEvent{{
		Event: kbruntime.VMEventAdded,
		VM: &vm.VMRecord{
			ID: "vm-1", Name: "example", State: vm.StateRunning,
			ObservedState: vm.ObservedStateRunning, Backend: "cloud-hypervisor",
		},
	}}
	if err := writeVMEventTable(&output, events, true); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"EVENT", "ADDED", "vm-1", "example", "RUNNING"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("event output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestWriteVMEventJSONLine(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	event := kbruntime.VMStatusEvent{
		Event: kbruntime.VMEventDeleted,
		VM:    &vm.VMRecord{ID: "vm-1", Name: "example"},
	}
	if err := writeJSONLine(&output, event); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("JSON event is not one NDJSON line: %q", output.String())
	}
	var decoded kbruntime.VMStatusEvent
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != kbruntime.VMEventDeleted || decoded.VM.ID != "vm-1" {
		t.Fatalf("decoded event = %+v", decoded)
	}
}

func TestFilterVMRecords(t *testing.T) {
	t.Parallel()

	records := []*vm.VMRecord{{ID: "vm-1", Name: "first"}, {ID: "vm-2", Name: "second"}}
	selected := filterVMRecords(records, []string{"second", "vm-1", "second", "missing"})
	if len(selected) != 2 || selected[0].ID != "vm-2" || selected[1].ID != "vm-1" {
		t.Fatalf("selected records = %+v", selected)
	}
}

func TestLogsCommandExposesFollowFlags(t *testing.T) {
	t.Parallel()

	cmd := newLogsCommand(&rootOptions{})
	for _, name := range []string{"follow", "interval", "source", "tail"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("logs flag %q is missing", name)
		}
	}
}
