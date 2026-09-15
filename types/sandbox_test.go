package types

import (
	"strings"
	"testing"
)

func TestSandboxID(t *testing.T) {
	id, err := NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseSandboxID(id.String()); err != nil || parsed != id {
		t.Fatalf("ParseSandboxID(%q) = %q, %v", id, parsed, err)
	}
	if id.String()[14] != '4' || !strings.ContainsRune("89ab", rune(id.String()[19])) {
		t.Fatalf("ID %q is not UUIDv4", id)
	}
}

func TestSandboxConfigValidationMatchesCreateContract(t *testing.T) {
	valid := SandboxConfig{Name: "agent.demo-1", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		config SandboxConfig
	}{
		{"name", SandboxConfig{Name: "bad/name", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage}},
		{"cpus", SandboxConfig{Name: "demo", Memory: MinSandboxMemory, Storage: MinSandboxStorage}},
		{"memory", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory - 1, Storage: MinSandboxStorage}},
		{"storage", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage - 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.config.Validate(); err == nil {
				t.Fatal("invalid spec passed validation")
			}
		})
	}
}
