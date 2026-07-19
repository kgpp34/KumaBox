package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

type lifecycleMetrics struct {
	started time.Time
	value   vmstore.PerformanceMetrics
}

func newLifecycleMetrics(operation string, started time.Time, rec *vmstore.VMRecord) *lifecycleMetrics {
	metrics := &lifecycleMetrics{started: started, value: vmstore.PerformanceMetrics{
		Operation:              operation,
		CommandStartedAt:       started.UTC(),
		EnvironmentFingerprint: environmentFingerprint(rec),
	}}
	if rec != nil && rec.Image != nil {
		metrics.value.ImageDigest = rec.Image.Digest
	}
	return metrics
}

func (m *lifecycleMetrics) bindRecord(rec *vmstore.VMRecord) {
	if rec == nil {
		return
	}
	m.value.EnvironmentFingerprint = environmentFingerprint(rec)
	if rec.Image != nil {
		m.value.ImageDigest = rec.Image.Digest
	}
}

func phaseTime(at time.Time) *time.Time {
	value := at.UTC()
	return &value
}

func (m *lifecycleMetrics) markImageResolved(at time.Time) {
	m.value.ImageResolvedAt = phaseTime(at)
}

func (m *lifecycleMetrics) markStorageReady(at time.Time) {
	m.value.StorageReadyAt = phaseTime(at)
}

func (m *lifecycleMetrics) markNetworkReady(at time.Time) {
	m.value.NetworkReadyAt = phaseTime(at)
}

func (m *lifecycleMetrics) markVMMSpawned(at time.Time) {
	m.value.VMMSpawnedAt = phaseTime(at)
}

func (m *lifecycleMetrics) markVMMAPIReady(at time.Time) {
	m.value.VMMAPIReadyAt = phaseTime(at)
}

func (m *lifecycleMetrics) markAgentConnected(at time.Time) {
	m.value.AgentConnectedAt = phaseTime(at)
}

func (m *lifecycleMetrics) markFirstExecCompleted(at time.Time) {
	m.value.FirstExecCompletedAt = phaseTime(at)
	m.value.ReadyDurationMs = at.Sub(m.started).Milliseconds()
}

func (m *lifecycleMetrics) snapshot() vmstore.PerformanceMetrics {
	return m.value
}

func environmentFingerprint(rec *vmstore.VMRecord) string {
	input := struct {
		GOOS      string
		GOARCH    string
		GoVersion string
		Kernel    string
		Backend   string
		CPUs      int
		Memory    int64
		Network   string
	}{
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		GoVersion: runtime.Version(),
		Kernel:    kernelRelease(),
		Backend:   backendName(rec),
	}
	if rec != nil {
		input.CPUs = rec.CPUs
		input.Memory = rec.MemoryBytes
		input.Network = rec.Network
	}
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func backendName(rec *vmstore.VMRecord) string {
	if rec == nil {
		return ""
	}
	return rec.Backend
}

func kernelRelease() string {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}
