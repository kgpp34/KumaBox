package runtime

import (
	"testing"

	"github.com/kumabox/kumabox/internal/backend"
)

func TestRequireRestoreModeFailsClosed(t *testing.T) {
	host := backend.NativeHost{BackendVersion: "51.0.0", RestoreModes: []string{"copy", "mmap"}}
	if err := requireRestoreMode(host, "copy"); err != nil {
		t.Fatal(err)
	}
	if err := requireRestoreMode(host, "mmap"); err != nil {
		t.Fatal(err)
	}
	if err := requireRestoreMode(host, "ondemand"); err == nil {
		t.Fatal("unadvertised mode was accepted")
	}
}
