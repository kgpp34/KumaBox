package snapshot

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound indicates that a snapshot reference did not resolve.
	ErrNotFound = errors.New("snapshot not found")
	// ErrNameConflict indicates that a snapshot name is already reserved.
	ErrNameConflict = errors.New("snapshot name conflict")
	// ErrAmbiguous indicates that an ID prefix resolves to multiple snapshots.
	ErrAmbiguous = errors.New("snapshot ref is ambiguous")
	// ErrInUse indicates that a reader or builder currently owns the snapshot lease.
	ErrInUse = errors.New("snapshot in use")
)

type snapshotIndex struct {
	SchemaVersion string             `json:"schemaVersion"`
	Snapshots     map[string]*Record `json:"snapshots"`
	Names         map[string]string  `json:"names"`
}

func (idx *snapshotIndex) init() {
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = "kumabox.snapshot.index.v1"
	}
	if idx.Snapshots == nil {
		idx.Snapshots = make(map[string]*Record)
	}
	if idx.Names == nil {
		idx.Names = make(map[string]string)
	}
}

func (idx *snapshotIndex) resolve(ref string) (string, error) {
	idx.init()
	if _, ok := idx.Snapshots[ref]; ok {
		return ref, nil
	}
	if id, ok := idx.Names[ref]; ok {
		return id, nil
	}
	if len(ref) < 3 {
		return "", fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	matched := ""
	for id := range idx.Snapshots {
		if !strings.HasPrefix(id, ref) {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("%w: %s", ErrAmbiguous, ref)
		}
		matched = id
	}
	if matched == "" {
		return "", fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return matched, nil
}
