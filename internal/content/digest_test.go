package content

import (
	"errors"
	"strings"
	"testing"
)

func TestSHA256(t *testing.T) {
	t.Parallel()

	digest := SHA256([]byte("abc"))
	const want = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := digest.String(); got != want {
		t.Fatalf("SHA256(abc) = %q, want %q", got, want)
	}
	if got := digest.Algorithm(); got != sha256Algorithm {
		t.Fatalf("Algorithm() = %q, want %q", got, sha256Algorithm)
	}
	if got := digest.Encoded(); got != strings.TrimPrefix(want, sha256Prefix) {
		t.Fatalf("Encoded() = %q", got)
	}
	if err := digest.Validate(); err != nil {
		t.Fatalf("generated digest is invalid: %v", err)
	}
}

func TestParseDigest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("01", sha256HexLength/2)
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "canonical", value: valid},
		{name: "zero", value: "", wantErr: true},
		{name: "unsupported", value: "sha512:" + strings.Repeat("0", 128), wantErr: true},
		{name: "short", value: "sha256:00", wantErr: true},
		{name: "uppercase", value: "sha256:" + strings.Repeat("A", sha256HexLength), wantErr: true},
		{name: "not hexadecimal", value: "sha256:" + strings.Repeat("z", sha256HexLength), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			digest, err := ParseDigest(test.value)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidDigest) {
					t.Fatalf("ParseDigest(%q) error = %v, want ErrInvalidDigest", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDigest(%q) error = %v", test.value, err)
			}
			if got := digest.String(); got != test.value {
				t.Fatalf("ParseDigest(%q).String() = %q", test.value, got)
			}
		})
	}
}

func TestZeroDigest(t *testing.T) {
	t.Parallel()

	var digest Digest
	if !digest.IsZero() {
		t.Fatal("zero Digest is not reported as zero")
	}
	if digest.Algorithm() != "" || digest.Encoded() != "" || digest.String() != "" {
		t.Fatal("zero Digest exposes non-empty data")
	}
	if !errors.Is(digest.Validate(), ErrInvalidDigest) {
		t.Fatalf("zero Digest validation error = %v, want ErrInvalidDigest", digest.Validate())
	}
}
