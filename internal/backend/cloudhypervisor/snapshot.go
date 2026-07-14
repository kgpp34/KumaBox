package cloudhypervisor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/vmstore"
)

// SnapshotVM asks Cloud Hypervisor to write its native paused VM state into
// destination. Writable disks are captured separately by runtime.
func (Backend) SnapshotVM(ctx context.Context, rec *vmstore.VMRecord, destination string) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve native snapshot destination: %w", err)
	}
	apiSocket, _, err := backendAPIConfig(rec)
	if err != nil {
		return err
	}
	destinationURL := (&url.URL{Scheme: "file", Path: abs}).String()
	return putJSONOnce(ctx, apiSocket, nativeSnapshotTimeout, "vm.snapshot", map[string]string{
		"destination_url": destinationURL,
	}, http.StatusNoContent)
}
