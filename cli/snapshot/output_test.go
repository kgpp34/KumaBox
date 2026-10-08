package snapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func TestSnapshotOutputIsIndentedAndTableHasHeaders(t *testing.T) {
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Snapshot{
		ID: types.SnapshotID("223e4567-e89b-42d3-a456-426614174000"), Name: "checkpoint",
		SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"), SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1, NetworkName: "default",
		},
		Size: 42, CreatedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	}
	var jsonOutput bytes.Buffer
	if err := writeJSON(&jsonOutput, record); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOutput.String(), "\n  \"id\"") || !strings.Contains(jsonOutput.String(), "\"cpus\": 2") {
		t.Fatalf("snapshot JSON = %q", jsonOutput.String())
	}
	var decoded output
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Config.Name != "box" || decoded.Config.NetworkName != "default" {
		t.Fatalf("snapshot output = %+v", decoded)
	}
	var table bytes.Buffer
	if err := writeTable(&table, []types.Snapshot{record}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"SNAPSHOT ID", "SANDBOX ID", record.ID.String(), "checkpoint", "1.0GiB"} {
		if !strings.Contains(table.String(), text) {
			t.Fatalf("snapshot table missing %q:\n%s", text, table.String())
		}
	}
}

func TestEmptySnapshotOutputsUseHeadersAndArray(t *testing.T) {
	var table bytes.Buffer
	if err := writeTable(&table, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(table.String(), "\n") != 1 {
		t.Fatalf("empty table = %q", table.String())
	}
	var jsonOutput bytes.Buffer
	if err := writeListJSON(&jsonOutput, nil); err != nil {
		t.Fatal(err)
	}
	if jsonOutput.String() != "[]\n" {
		t.Fatalf("empty JSON = %q", jsonOutput.String())
	}
}
