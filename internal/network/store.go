package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
)

const indexSchemaVersion = "kumabox.network.index.v1"
const leaseSchemaVersion = "kumabox.network.leases.v1"
const hostTapSchemaVersion = "kumabox.network.hostTap.v1"

type Store struct {
	indexPath   string
	leasePath   string
	leaseLock   string
	hostTapPath string
	hostTapLock string
}

type index struct {
	SchemaVersion string             `json:"schemaVersion"`
	Networks      map[string]*Record `json:"networks"`
}

type Lease struct {
	VMID      string    `json:"vmId"`
	MAC       string    `json:"mac"`
	TAP       string    `json:"tap"`
	CreatedAt time.Time `json:"createdAt"`
}

type leaseIndex struct {
	SchemaVersion string            `json:"schemaVersion"`
	CIDR          string            `json:"cidr"`
	Leases        map[string]*Lease `json:"leases"`
}

func NewStore(rootDir string) *Store {
	networkDir := filepath.Join(rootDir, "network")
	return &Store{
		indexPath:   filepath.Join(networkDir, "index.json"),
		leasePath:   filepath.Join(networkDir, "leases.json"),
		leaseLock:   filepath.Join(networkDir, "leases.lock"),
		hostTapPath: filepath.Join(networkDir, "host-tap.json"),
		hostTapLock: filepath.Join(networkDir, "host-tap.lock"),
	}
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

func (s *Store) ListLeases() (map[string]Lease, error) {
	leases, err := s.readLeases()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Lease, len(leases.Leases))
	for ip, lease := range leases.Leases {
		if lease != nil {
			out[ip] = *lease
		}
	}
	return out, nil
}

func (s *Store) ReadHostTapState() (*HostTapState, error) {
	return s.readHostTapState()
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

func (s *Store) readLeases() (*leaseIndex, error) {
	raw, err := os.ReadFile(s.leasePath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &leaseIndex{
				SchemaVersion: leaseSchemaVersion,
				Leases:        map[string]*Lease{},
			}, nil
		}
		return nil, fmt.Errorf("read network leases: %w", err)
	}

	var leases leaseIndex
	if err := json.Unmarshal(raw, &leases); err != nil {
		return nil, fmt.Errorf("parse network leases: %w", err)
	}
	if leases.SchemaVersion != "" && leases.SchemaVersion != leaseSchemaVersion {
		return nil, fmt.Errorf("unsupported network leases schema %q", leases.SchemaVersion)
	}
	if leases.SchemaVersion == "" {
		leases.SchemaVersion = leaseSchemaVersion
	}
	if leases.Leases == nil {
		leases.Leases = map[string]*Lease{}
	}
	return &leases, nil
}

func (s *Store) writeLeases(leases *leaseIndex) error {
	if leases.SchemaVersion == "" {
		leases.SchemaVersion = leaseSchemaVersion
	}
	if leases.Leases == nil {
		leases.Leases = map[string]*Lease{}
	}
	if err := fileutil.WriteJSONAtomic(s.leasePath, leases, ".leases-*.tmp"); err != nil {
		return fmt.Errorf("write network leases: %w", err)
	}
	return nil
}

func (s *Store) lockLeases() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.leaseLock), 0o755); err != nil {
		return nil, fmt.Errorf("create network lease lock dir: %w", err)
	}

	file, err := os.OpenFile(s.leaseLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open network lease lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock network leases: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (s *Store) readHostTapState() (*HostTapState, error) {
	raw, err := os.ReadFile(s.hostTapPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read host-tap state: %w", err)
	}

	var state HostTapState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse host-tap state: %w", err)
	}
	if state.SchemaVersion != "" && state.SchemaVersion != hostTapSchemaVersion {
		return nil, fmt.Errorf("unsupported host-tap schema %q", state.SchemaVersion)
	}
	if state.SchemaVersion == "" {
		state.SchemaVersion = hostTapSchemaVersion
	}
	return &state, nil
}

func (s *Store) writeHostTapState(state *HostTapState) error {
	if state.SchemaVersion == "" {
		state.SchemaVersion = hostTapSchemaVersion
	}
	if err := fileutil.WriteJSONAtomic(s.hostTapPath, state, ".host-tap-*.tmp"); err != nil {
		return fmt.Errorf("write host-tap state: %w", err)
	}
	return nil
}

func (s *Store) removeHostTapState() error {
	if err := os.Remove(s.hostTapPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove host-tap state: %w", err)
	}
	return nil
}

func (s *Store) lockHostTap() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.hostTapLock), 0o755); err != nil {
		return nil, fmt.Errorf("create host-tap lock dir: %w", err)
	}

	file, err := os.OpenFile(s.hostTapLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open host-tap lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock host-tap: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
