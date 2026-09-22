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
		{"NIC count", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: MaxSandboxNICs + 1}},
		{"network without NIC", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NetworkName: "default"}},
		{"network name", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: 1, NetworkName: "bad/name"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if err == nil {
				t.Fatal("invalid spec passed validation")
			}
			if strings.Contains(err.Error(), "--") {
				t.Fatalf("domain validation leaked CLI flag syntax: %v", err)
			}
		})
	}
}

func TestCommandValidation(t *testing.T) {
	command := Command{Args: []string{"sh", "-c", "echo"}, Env: map[string]string{"A": "2", "EMPTY": ""}}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Command{
		{},
		{Args: []string{""}},
		{Args: []string{"echo", "bad\x00argument"}},
		{Args: []string{"env"}, Env: map[string]string{"": "missing-key"}},
		{Args: []string{"env"}, Env: map[string]string{"BAD=KEY": "value"}},
		{Args: []string{"env"}, Env: map[string]string{"BAD\x00KEY": "value"}},
		{Args: []string{"env"}, Env: map[string]string{"KEY": "bad\x00value"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted invalid command %#v", invalid)
		}
	}
}
