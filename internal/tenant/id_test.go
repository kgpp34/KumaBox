package tenant

import (
	"errors"
	"strings"
	"testing"
)

func TestNewID(t *testing.T) {
	t.Parallel()

	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("generated ID is invalid: %v", err)
	}
	if got, want := len(id.String()), len(idPrefix)+generatedBytes*2; got != want {
		t.Fatalf("len(NewID()) = %d, want %d", got, want)
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "generated shape", value: "tenant_0123456789abcdef0123456789abcdef"},
		{name: "external name", value: "tenant_control-plane-1"},
		{name: "zero", value: "", wantErr: true},
		{name: "missing prefix", value: "control-plane", wantErr: true},
		{name: "empty suffix", value: "tenant_", wantErr: true},
		{name: "uppercase", value: "tenant_Control", wantErr: true},
		{name: "leading hyphen", value: "tenant_-control", wantErr: true},
		{name: "trailing hyphen", value: "tenant_control-", wantErr: true},
		{name: "separator", value: "tenant_control/plane", wantErr: true},
		{name: "too long", value: "tenant_" + strings.Repeat("a", maxIDSuffixLen+1), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			id, err := ParseID(test.value)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidID) {
					t.Fatalf("ParseID(%q) error = %v, want ErrInvalidID", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseID(%q) error = %v", test.value, err)
			}
			if got := id.String(); got != test.value {
				t.Fatalf("ParseID(%q).String() = %q", test.value, got)
			}
			if err := id.Validate(); err != nil {
				t.Fatalf("parsed ID is invalid: %v", err)
			}
		})
	}
}

func TestZeroID(t *testing.T) {
	t.Parallel()

	var id ID
	if !id.IsZero() {
		t.Fatal("zero ID is not reported as zero")
	}
	if !errors.Is(id.Validate(), ErrInvalidID) {
		t.Fatalf("zero ID validation error = %v, want ErrInvalidID", id.Validate())
	}
}
