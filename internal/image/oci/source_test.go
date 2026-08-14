// SPDX-License-Identifier: MIT

package oci

import (
	"context"
	"strings"
	"testing"
)

func TestOpenRejectsUnknownSource(t *testing.T) {
	t.Parallel()

	_, err := openSource(context.Background(), SourceRequest{Ref: "example.com/image:latest", Source: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "OCI_SOURCE_INVALID") {
		t.Fatalf("expected source validation error, got %v", err)
	}
}
