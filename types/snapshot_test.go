package types

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotValidation(t *testing.T) {
	id, err := NewSnapshotID()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		ID: id, Name: "release/one:ready", SandboxID: SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		SourceGeneration: 4, ImageDigest: digest, VMM: VMMCloudHypervisor,
		Config:    SandboxConfig{Name: "box", CPUs: 1, Memory: DefaultSandboxMemory, Storage: DefaultSandboxStorage},
		CreatedAt: time.Now().UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot.Name = "bad name"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Snapshot.Validate accepted an invalid name")
	}
}
