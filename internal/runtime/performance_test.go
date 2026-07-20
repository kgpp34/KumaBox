package runtime

import (
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestLifecycleMetricsRecordOrderedReadinessPhases(t *testing.T) {
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
	readyAt := apiAt.Add(10 * time.Millisecond)
	metrics.markImageResolved(imageAt)
	metrics.markStorageReady(storageAt)
	metrics.markNetworkReady(networkAt)
	metrics.markVMMSpawned(vmmAt)
	metrics.markVMMAPIReady(apiAt)
	metrics.markAgentConnected(readyAt)
	metrics.markFirstExecCompleted(readyAt)

	got := metrics.snapshot()
	if got.ImageDigest != "sha256:image" || got.EnvironmentFingerprint == "" {
		t.Fatalf("identity metrics = %+v", got)
	}
	if got.ReadyDurationMs < 50 {
		t.Fatalf("ready duration = %dms, want at least 50ms", got.ReadyDurationMs)
	}
	if got.VMMAPIReadyDurationMs < 40 || got.AgentReadyDurationMs < 50 || got.FirstExecDurationMs < 50 {
		t.Fatalf("phase durations = %+v", got)
	}
	if got.ImageResolvedAt == nil || got.FirstExecCompletedAt == nil {
		t.Fatalf("missing phase timestamps = %+v", got)
	}
	if !got.ImageResolvedAt.Before(*got.FirstExecCompletedAt) {
		t.Fatalf("phase timestamps are not ordered: %+v", got)
	}
}

func TestRequiresAgentReadinessOnlyForDirectImages(t *testing.T) {
	if !requiresAgentReadiness(&vmstore.VMRecord{Image: &vmstore.ImageRef{BootMode: "direct"}}) {
		t.Fatal("direct image should require guest readiness")
	}
	if requiresAgentReadiness(&vmstore.VMRecord{Image: &vmstore.ImageRef{BootMode: "uefi"}}) {
		t.Fatal("UEFI image should not require direct guest readiness")
	}
}
