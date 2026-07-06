package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const indexSchemaVersion = "kumabox.network.index.v1"

type Store struct {
	indexPath string
}

type index struct {
	SchemaVersion string             `json:"schemaVersion"`
	Networks      map[string]*Record `json:"networks"`
}

func NewStore(rootDir string) *Store {
	return &Store{indexPath: filepath.Join(rootDir, "network", "index.json")}
}

func (s *Store) List() ([]Record, error) {
	idx, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(idx.Networks))
	for _, rec := range idx.Networks {
		if rec != nil {
			records = append(records, *rec)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func (s *Store) Inspect(vmID string) (*InspectResult, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	result := &InspectResult{VMID: vmID, Interfaces: []Record{}}
	for _, rec := range records {
		if rec.VMID == vmID {
			result.Interfaces = append(result.Interfaces, rec)
		}
	}
	return result, nil
}

func (s *Store) readIndex() (*index, error) {
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &index{
				SchemaVersion: indexSchemaVersion,
				Networks:      map[string]*Record{},
			}, nil
		}
		return nil, fmt.Errorf("read network index: %w", err)
	}

	var idx index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse network index: %w", err)
	}
	if idx.SchemaVersion != "" && idx.SchemaVersion != indexSchemaVersion {
		return nil, fmt.Errorf("unsupported network index schema %q", idx.SchemaVersion)
	}
	if idx.Networks == nil {
		idx.Networks = map[string]*Record{}
	}
	return &idx, nil
}
