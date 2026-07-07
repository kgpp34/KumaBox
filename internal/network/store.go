package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
)

const indexSchemaVersion = "kumabox.network.index.v1"
const leaseSchemaVersion = "kumabox.network.leases.v1"
const hostTapSchemaVersion = "kumabox.network.hostTap.v1"

// Store persists network provider state under a KumaBox root directory.
//
// The store owns three related files: provider records, IP leases, and global
// host-tap bridge ownership. Each file has its own flock because lifecycle and
// network commands may touch them independently.
type Store struct {
	indexPath   string
	indexLock   string
	leasePath   string
	leaseLock   string
	hostTapPath string
	hostTapLock string
}

type index struct {
	SchemaVersion string             `json:"schemaVersion"`
	Networks      map[string]*Record `json:"networks"`
}

// Lease records exclusive ownership of one guest IP address.
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

// NewStore returns a network store rooted under rootDir.
func NewStore(rootDir string) *Store {
	networkDir := filepath.Join(rootDir, "network")
	return &Store{
		indexPath:   filepath.Join(networkDir, "index.json"),
		indexLock:   filepath.Join(networkDir, "index.lock"),
		leasePath:   filepath.Join(networkDir, "leases.json"),
		leaseLock:   filepath.Join(networkDir, "leases.lock"),
		hostTapPath: filepath.Join(networkDir, "host-tap.json"),
		hostTapLock: filepath.Join(networkDir, "host-tap.lock"),
	}
}

// List returns provider records sorted by creation time.
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

// UpsertRecord inserts or replaces one provider record.
//
// Callers use this after the host-side device exists, so inspect can treat the
// record as the source of provider truth.
func (s *Store) UpsertRecord(rec Record) error {
	if rec.ID == "" {
		return fmt.Errorf("network record id must not be empty")
	}
	unlock, err := s.lockIndex()
	if err != nil {
		return err
	}
	defer unlock()
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	idx.Networks[rec.ID] = &rec
	return s.writeIndex(idx)
}

// DeleteRecord removes a provider record.
//
// Device and lease cleanup must be completed before this call; otherwise the
// metadata needed for a later cleanup retry would be lost.
func (s *Store) DeleteRecord(id string) error {
	if id == "" {
		return nil
	}
	unlock, err := s.lockIndex()
	if err != nil {
		return err
	}
	defer unlock()
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	delete(idx.Networks, id)
	return s.writeIndex(idx)
}

// MarkCleanupPending records a failed provider cleanup attempt.
//
// Missing records are ignored so delete paths can be retried after partial
// cleanup without turning "already gone" into a hard failure.
func (s *Store) MarkCleanupPending(id, reason string) error {
	if id == "" {
		return nil
	}
	unlock, err := s.lockIndex()
	if err != nil {
		return err
	}
	defer unlock()
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	rec, ok := idx.Networks[id]
	if !ok || rec == nil {
		return nil
	}
	now := time.Now().UTC()
	rec.Cleanup = Cleanup{
		Pending:       true,
		Reason:        reason,
		LastAttemptAt: now.Format(time.RFC3339Nano),
	}
	rec.UpdatedAt = now
	return s.writeIndex(idx)
}

// Inspect returns provider state for a VM ID without VM-record comparison.
//
// Most CLI calls should prefer InspectVM so drift can be reported.
func (s *Store) Inspect(vmID string) (*InspectResult, error) {
	return s.InspectVM(vmID, "", "", nil)
}

// InspectVM compares provider records with the VM's persisted network configs.
func (s *Store) InspectVM(vmID, vmName, network string, configs []Config) (*InspectResult, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	result := &InspectResult{
		VMID:       vmID,
		VMName:     vmName,
		Network:    network,
		Interfaces: []Record{},
		VMConfigs:  cloneConfigs(configs),
	}
	for _, rec := range records {
		if rec.VMID == vmID {
			result.Interfaces = append(result.Interfaces, rec)
		}
	}
	result.Drift = inspectDrift(result.Interfaces, configs)
	return result, nil
}

func inspectDrift(records []Record, configs []Config) []string {
	drift := []string{}
	recordsByID := make(map[string]Record, len(records))
	for _, rec := range records {
		if rec.ID != "" {
			recordsByID[rec.ID] = rec
		}
	}
	configsByID := make(map[string]Config, len(configs))
	for _, cfg := range configs {
		if cfg.ID != "" {
			configsByID[cfg.ID] = cfg
		}
	}
	for _, cfg := range configs {
		if cfg.ID == "" {
			drift = append(drift, "VM network config is missing id")
			continue
		}
		rec, ok := recordsByID[cfg.ID]
		if !ok {
			drift = append(drift, fmt.Sprintf("VM network config %s is missing provider record", cfg.ID))
			continue
		}
		drift = appendDriftMismatch(drift, cfg.ID, "tap", cfg.TAP, rec.TAP)
		drift = appendDriftMismatch(drift, cfg.ID, "mac", cfg.MAC, rec.MAC)
		drift = appendDriftMismatch(drift, cfg.ID, "backend", cfg.Backend, rec.Provider)
		drift = appendDriftMismatch(drift, cfg.ID, "bridgeDev", cfg.BridgeDev, rec.BridgeDev)
		if cfg.Network == nil {
			if len(rec.IPs) > 0 || rec.Gateway != "" || len(rec.DNS) > 0 {
				drift = append(drift, fmt.Sprintf("VM network config %s is missing guest network details", cfg.ID))
			}
			continue
		}
		drift = appendDriftMismatch(drift, cfg.ID, "ip", configIPCIDR(cfg), firstString(rec.IPs))
		drift = appendDriftMismatch(drift, cfg.ID, "gateway", cfg.Network.Gateway, rec.Gateway)
		if !reflect.DeepEqual(cfg.Network.DNS, rec.DNS) {
			drift = append(drift, fmt.Sprintf("network %s dns mismatch: vm=%v provider=%v", cfg.ID, cfg.Network.DNS, rec.DNS))
		}
	}
	for _, rec := range records {
		if rec.ID == "" {
			drift = append(drift, fmt.Sprintf("provider record for tap %s is missing id", rec.TAP))
			continue
		}
		if _, ok := configsByID[rec.ID]; !ok {
			drift = append(drift, fmt.Sprintf("provider record %s is missing from VM record", rec.ID))
		}
	}
	return drift
}

func appendDriftMismatch(drift []string, id, field, vmValue, providerValue string) []string {
	if vmValue == providerValue {
		return drift
	}
	return append(drift, fmt.Sprintf("network %s %s mismatch: vm=%q provider=%q", id, field, vmValue, providerValue))
}

func configIPCIDR(cfg Config) string {
	if cfg.Network == nil || cfg.Network.IP == "" {
		return ""
	}
	if cfg.Network.Prefix <= 0 {
		return cfg.Network.IP
	}
	return fmt.Sprintf("%s/%d", cfg.Network.IP, cfg.Network.Prefix)
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func cloneConfigs(configs []Config) []Config {
	if len(configs) == 0 {
		return nil
	}
	copied := make([]Config, len(configs))
	copy(copied, configs)
	for i := range copied {
		if configs[i].Network == nil {
			continue
		}
		network := *configs[i].Network
		network.DNS = append([]string(nil), configs[i].Network.DNS...)
		copied[i].Network = &network
	}
	return copied
}

// ListLeases returns a defensive copy of the IP lease map keyed by IP address.
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

// ReadHostTapState returns the global host-tap state, if it exists.
func (s *Store) ReadHostTapState() (*HostTapState, error) {
	return s.readHostTapState()
}

// IncrementHostTapRef increases the number of VM attachments using host-tap.
//
// The state file must already exist; setup is responsible for creating it
// before VM network attachment proceeds.
func (s *Store) IncrementHostTapRef(count int) error {
	if count <= 0 {
		return nil
	}
	return s.adjustHostTapRef(count, true)
}

// DecrementHostTapRef decreases the host-tap attachment count.
//
// Missing state is treated as already cleaned up to keep delete idempotent.
func (s *Store) DecrementHostTapRef(count int) error {
	if count <= 0 {
		return nil
	}
	return s.adjustHostTapRef(-count, false)
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

func (s *Store) writeIndex(idx *index) error {
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = indexSchemaVersion
	}
	if idx.Networks == nil {
		idx.Networks = map[string]*Record{}
	}
	if err := fileutil.WriteJSONAtomic(s.indexPath, idx, ".index-*.tmp"); err != nil {
		return fmt.Errorf("write network index: %w", err)
	}
	return nil
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

func (s *Store) lockIndex() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.indexLock), 0o755); err != nil {
		return nil, fmt.Errorf("create network index lock dir: %w", err)
	}

	file, err := os.OpenFile(s.indexLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open network index lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock network index: %w", err)
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

func (s *Store) adjustHostTapRef(delta int, requireState bool) error {
	unlock, err := s.lockHostTap()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.readHostTapState()
	if err != nil {
		return err
	}
	if state == nil {
		if requireState {
			return fmt.Errorf("host-tap state is missing")
		}
		return nil
	}
	state.RefCount += delta
	if state.RefCount < 0 {
		state.RefCount = 0
	}
	state.UpdatedAt = time.Now().UTC()
	return s.writeHostTapState(state)
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
