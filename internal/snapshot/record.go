// Package snapshot persists portable stopped-snapshot metadata and coordinates
// access to snapshot payloads across daemonless KumaBox commands.
package snapshot

import "time"

// State is the durable publication state of a snapshot.
type State string

const (
	StatePending  State = "pending"
	StateReady    State = "ready"
	StateDeleting State = "deleting"
)

// Record is the compact snapshot entry stored in the global index.
type Record struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	State          State           `json:"state"`
	DataDir        string          `json:"dataDir"`
	StagingDir     string          `json:"stagingDir,omitempty"`
	SizeBytes      int64           `json:"sizeBytes,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	LastAccessedAt time.Time       `json:"lastAccessedAt"`
	Performance    *CaptureMetrics `json:"performance,omitempty"`
}

// CaptureMetrics separates the guest pause window from work that can happen
// after the VM has resumed.
type CaptureMetrics struct {
	PauseDurationMs       int64 `json:"pauseDurationMs"`
	NativeCaptureMs       int64 `json:"nativeCaptureMs"`
	WritableDiskStageMs   int64 `json:"writableDiskStageMs"`
	PublicationDurationMs int64 `json:"publicationDurationMs"`
	TotalDurationMs       int64 `json:"totalDurationMs"`
}

func cloneRecord(rec *Record) *Record {
	if rec == nil {
		return nil
	}
	cloned := *rec
	if rec.Performance != nil {
		metrics := *rec.Performance
		cloned.Performance = &metrics
	}
	return &cloned
}
