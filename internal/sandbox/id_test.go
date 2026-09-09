package sandbox

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
	if got, want := len(id.String()), len(idPrefix)+idEncodedLength; got != want {
		t.Fatalf("len(NewID()) = %d, want %d", got, want)
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()

	valid := idPrefix + strings.Repeat("01", idRandomBytes)
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "canonical", value: valid},
		{name: "zero", value: "", wantErr: true},
		{name: "wrong prefix", value: "vm_" + strings.Repeat("0", idEncodedLength), wantErr: true},
		{name: "short", value: idPrefix + strings.Repeat("0", idEncodedLength-1), wantErr: true},
		{name: "uppercase", value: idPrefix + strings.Repeat("A", idEncodedLength), wantErr: true},
		{name: "not hexadecimal", value: idPrefix + strings.Repeat("z", idEncodedLength), wantErr: true},
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
