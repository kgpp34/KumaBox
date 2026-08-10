package runtime

import (
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestLifecycleMetricsUseVMMAPIAsLifecycleReadiness(t *testing.T) {
	started := time.Now()
	metrics := newLifecycleMetrics("run", started, &vmstore.VMRecord{
		Backend: "cloud-hypervisor",
		CPUs:    1,
		Image:   &vmstore.ImageRef{Digest: "sha256:image"},
	})
	imageAt := started.Add(10 * time.Millisecond)
	storageAt := imageAt.Add(10 * time.Millisecond)
	networkAt := storageAt.Add(10 * time.Millisecond)
	vmmAt := networkAt.Add(10 * time.Millisecond)
	apiAt := vmmAt.Add(10 * time.Millisecond)
	metrics.markImageResolved(imageAt)
	metrics.markStorageReady(storageAt)
	metrics.markNetworkReady(networkAt)
	metrics.markVMMSpawned(vmmAt)
	metrics.markVMMAPIReady(apiAt)

	got := metrics.snapshot()
	if got.ImageDigest != "sha256:image" || got.EnvironmentFingerprint == "" {
		t.Fatalf("identity metrics = %+v", got)
	}
	if got.ReadyDurationMs < 40 {
		t.Fatalf("ready duration = %dms, want at least 40ms", got.ReadyDurationMs)
	}
	if got.VMMAPIReadyDurationMs < 40 || got.ReadyDurationMs != got.VMMAPIReadyDurationMs {
		t.Fatalf("phase durations = %+v", got)
	}
	if got.ImageResolvedAt == nil || got.VMMAPIReadyAt == nil {
		t.Fatalf("missing phase timestamps = %+v", got)
	}
	if !got.ImageResolvedAt.Before(*got.VMMAPIReadyAt) {
		t.Fatalf("phase timestamps are not ordered: %+v", got)
	}
}
