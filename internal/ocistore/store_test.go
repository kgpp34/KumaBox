// SPDX-License-Identifier: MIT

package ocistore

import (
	"strings"
	"testing"
)

func TestSplitDigest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("a", 64)
	algo, value, err := splitDigest(valid)
	if err != nil {
		t.Fatalf("splitDigest returned error: %v", err)
	}
	if algo != "sha256" || value != strings.Repeat("a", 64) {
		t.Fatalf("splitDigest = %q %q", algo, value)
	}

	for _, digest := range []string{
		"",
		"sha256:",
		"sha512:" + strings.Repeat("a", 128),
		"sha256:not-hex",
		"sha256:" + strings.Repeat("a", 63),
	} {
		if _, _, err := splitDigest(digest); err == nil {
			t.Fatalf("splitDigest(%q) expected error", digest)
		}
	}
}
