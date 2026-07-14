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
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	State          State     `json:"state"`
	DataDir        string    `json:"dataDir"`
	StagingDir     string    `json:"stagingDir,omitempty"`
	SizeBytes      int64     `json:"sizeBytes,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	LastAccessedAt time.Time `json:"lastAccessedAt"`
}

func cloneRecord(rec *Record) *Record {
	if rec == nil {
		return nil
	}
	cloned := *rec
	return &cloned
}
