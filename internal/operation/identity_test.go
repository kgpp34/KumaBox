package operation

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
		{name: "wrong prefix", value: "kb_" + strings.Repeat("0", idEncodedLength), wantErr: true},
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

func TestNewRequestID(t *testing.T) {
	t.Parallel()

	id, err := NewRequestID()
	if err != nil {
		t.Fatalf("NewRequestID() error = %v", err)
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("generated request ID is invalid: %v", err)
	}
	value := id.String()
	isVersionFour := len(value) == 36 && value[14] == '4'
	isRFCVariant := len(value) == 36 && strings.ContainsRune("89ab", rune(value[19]))
	if !isVersionFour || !isRFCVariant {
		t.Fatalf("NewRequestID() = %q, want canonical UUIDv4", value)
	}
}

func TestParseRequestID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "uuid", value: "2f1c6a8e-31db-4f26-856a-dc55e84ce551"},
		{name: "control plane key", value: "control-plane.request_001"},
		{name: "zero", value: "", wantErr: true},
		{name: "uppercase", value: "REQUEST-1", wantErr: true},
		{name: "leading separator", value: "-request", wantErr: true},
		{name: "trailing separator", value: "request-", wantErr: true},
		{name: "slash", value: "control/request", wantErr: true},
		{name: "space", value: "control request", wantErr: true},
		{name: "too long", value: strings.Repeat("a", maxRequestIDLength+1), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			id, err := ParseRequestID(test.value)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidRequestID) {
					t.Fatalf("ParseRequestID(%q) error = %v, want ErrInvalidRequestID", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRequestID(%q) error = %v", test.value, err)
			}
			if got := id.String(); got != test.value {
				t.Fatalf("ParseRequestID(%q).String() = %q", test.value, got)
			}
		})
	}
}

func TestZeroRequestID(t *testing.T) {
	t.Parallel()

	var id RequestID
	if !id.IsZero() {
		t.Fatal("zero RequestID is not reported as zero")
	}
	if !errors.Is(id.Validate(), ErrInvalidRequestID) {
		t.Fatalf("zero RequestID validation error = %v, want ErrInvalidRequestID", id.Validate())
	}
}
