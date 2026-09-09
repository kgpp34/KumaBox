package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestParseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "simple", value: "agent"},
		{name: "numbered", value: "agent-001"},
		{name: "single character", value: "a"},
		{name: "zero", value: "", wantErr: true},
		{name: "uppercase", value: "Agent", wantErr: true},
		{name: "leading hyphen", value: "-agent", wantErr: true},
		{name: "trailing hyphen", value: "agent-", wantErr: true},
		{name: "underscore", value: "agent_1", wantErr: true},
		{name: "dot", value: "agent.local", wantErr: true},
		{name: "too long", value: strings.Repeat("a", maxNameLength+1), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			name, err := ParseName(test.value)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidName) {
					t.Fatalf("ParseName(%q) error = %v, want ErrInvalidName", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseName(%q) error = %v", test.value, err)
			}
			if got := name.String(); got != test.value {
				t.Fatalf("ParseName(%q).String() = %q", test.value, got)
			}
		})
	}
}

func TestZeroName(t *testing.T) {
	t.Parallel()

	var name Name
	if !name.IsZero() {
		t.Fatal("zero Name is not reported as zero")
	}
	if !errors.Is(name.Validate(), ErrInvalidName) {
		t.Fatalf("zero Name validation error = %v, want ErrInvalidName", name.Validate())
	}
}
