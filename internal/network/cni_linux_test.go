//go:build linux

package network

import (
	"strings"
	"testing"
)

func TestPrepareCNINetnsRejectsMissingUnmanagedPath(t *testing.T) {
	path := t.TempDir() + "/missing"
	_, _, err := prepareCNINetnsLinux("kb_test", path)
	if err == nil || !strings.Contains(err.Error(), "is not managed") {
		t.Fatalf("prepare error = %v, want unmanaged path rejection", err)
	}
}
