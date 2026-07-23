package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
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
	engine        meta.MetaEngine
	leaseEngine   meta.MetaEngine
	hostTapEngine meta.MetaEngine
	indexPath     string
	indexLock     string
	leasePath     string
	leaseLock     string
	hostTapPath   string
	hostTapLock   string
}

var (
	networkIndexCollection = meta.NewCollection[networkIndex]("networks", networkIndexTable)
	leaseIndexCollection   = meta.NewCollection[leaseIndex]("leases", networkLeaseTable)
	hostTapCollection      = meta.NewCollection[HostTapState]("host-tap", hostTapTable)
)

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
	return NewStoreWithEngines(rootDir,
		mustOpenNetworkEngine(metajson.Namespace{Name: "networks", FilePath: filepath.Join(networkDir, "index.json"), LockPath: filepath.Join(networkDir, "index.lock"), Codec: indexCodec{}}),
		mustOpenNetworkEngine(metajson.Namespace{Name: "leases", FilePath: filepath.Join(networkDir, "leases.json"), LockPath: filepath.Join(networkDir, "leases.lock"), Codec: leaseCodec{}}),
		mustOpenNetworkEngine(metajson.Namespace{Name: "host-tap", FilePath: filepath.Join(networkDir, "host-tap.json"), LockPath: filepath.Join(networkDir, "host-tap.lock"), Codec: hostTapCodec{}}),
	)
}

// NewStoreWithEngines creates a network store with separately injectable
// engines for provider records, leases, and host-tap ownership.
func NewStoreWithEngines(rootDir string, engine, leaseEngine, hostTapEngine meta.MetaEngine) *Store {
	networkDir := filepath.Join(rootDir, "network")
	return &Store{
		engine:        engine,
		leaseEngine:   leaseEngine,
		hostTapEngine: hostTapEngine,
		indexPath:     filepath.Join(networkDir, "index.json"),
		indexLock:     filepath.Join(networkDir, "index.lock"),
		leasePath:     filepath.Join(networkDir, "leases.json"),
		leaseLock:     filepath.Join(networkDir, "leases.lock"),
		hostTapPath:   filepath.Join(networkDir, "host-tap.json"),
		hostTapLock:   filepath.Join(networkDir, "host-tap.lock"),
	}
}

// MetadataEngines exposes network persistence boundaries to migration tools.
func (s *Store) MetadataEngines() (meta.MetaEngine, meta.MetaEngine, meta.MetaEngine) {
	return s.engine, s.leaseEngine, s.hostTapEngine
}

func mustOpenNetworkEngine(namespace metajson.Namespace) meta.MetaEngine {
	engine, err := metajson.Open(namespace)
	if err != nil {
		panic(fmt.Sprintf("open network metadata engine: %v", err))
	}
	return engine
}

// List returns provider records sorted by creation time.
func (s *Store) List() ([]Record, error) {
	var records []Record
	err := s.withIndex(false, func(idx *networkIndex) error {
		records = make([]Record, 0, len(idx.Networks))
		for _, rec := range idx.Networks {
			if rec != nil {
				records = append(records, *rec)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	return s.withIndex(true, func(idx *networkIndex) error { idx.Networks[rec.ID] = &rec; return nil })
}

// DeleteRecord removes a provider record.
//
// Device and lease cleanup must be completed before this call; otherwise the
// metadata needed for a later cleanup retry would be lost.
func (s *Store) DeleteRecord(id string) error {
	if id == "" {
		return nil
	}
	return s.withIndex(true, func(idx *networkIndex) error { delete(idx.Networks, id); return nil })
}

// MarkCleanupPending records a failed provider cleanup attempt.
//
// Missing records are ignored so delete paths can be retried after partial
// cleanup without turning "already gone" into a hard failure.
func (s *Store) MarkCleanupPending(id, reason string) error {
	if id == "" {
		return nil
	}
	return s.withIndex(true, func(idx *networkIndex) error {
		rec, ok := idx.Networks[id]
		if !ok || rec == nil {
			return nil
		}
		now := time.Now().UTC()
		rec.Cleanup = Cleanup{Pending: true, Reason: reason, LastAttemptAt: now.Format(time.RFC3339Nano)}
		rec.UpdatedAt = now
		return nil
	})
}

// Inspect returns provider state for a VM ID without VM-record comparison.
//
// Most CLI calls should prefer InspectVM so drift can be reported.
func (s *Store) Inspect(vmID string) (*InspectResult, error) {
	return s.InspectVM(vmID, "", "", nil, nil)
}

// InspectVM compares provider records with the VM's persisted network configs.
func (s *Store) InspectVM(vmID, vmName, network string, networks []string, configs []Config) (*InspectResult, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	result := &InspectResult{
		VMID:       vmID,
		VMName:     vmName,
		Network:    network,
		Networks:   append([]string(nil), networks...),
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
		drift = appendDriftMismatch(drift, cfg.ID, "networkName", cfg.NetworkName, rec.Network)
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
	var out map[string]Lease
	err := s.withLeases(false, func(leases *leaseIndex) error {
		out = make(map[string]Lease, len(leases.Leases))
		for ip, lease := range leases.Leases {
			if lease != nil {
				out[ip] = *lease
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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

func (s *Store) withIndex(write bool, fn func(*networkIndex) error) error {
	ctx := context.Background()
	if write {
		return s.engine.Update(ctx, meta.Scope{Write: "networks"}, meta.CommitDurable, func(writer meta.Writer) error {
			idx, err := s.readNetworkIndex(ctx, writer)
			if err != nil {
				return err
			}
			if err := fn(idx); err != nil {
				return err
			}
			return networkIndexCollection.Upsert(ctx, writer, networkIndexRecord, idx)
		})
	}
	return s.engine.View(ctx, []meta.Namespace{"networks"}, func(reader meta.Reader) error {
		idx, err := s.readNetworkIndex(ctx, reader)
		if err != nil {
			return err
		}
		return fn(idx)
	})
}

func (s *Store) readNetworkIndex(ctx context.Context, reader meta.Reader) (*networkIndex, error) {
	idx, err := networkIndexCollection.Get(ctx, reader, networkIndexRecord)
	if errors.Is(err, meta.ErrNotFound) {
		idx = &networkIndex{SchemaVersion: indexSchemaVersion, Networks: map[string]*Record{}}
	} else if err != nil {
		return nil, fmt.Errorf("read network index: %w", err)
	}
	if idx.SchemaVersion != "" && idx.SchemaVersion != indexSchemaVersion {
		return nil, fmt.Errorf("unsupported network index schema %q", idx.SchemaVersion)
	}
	if idx.Networks == nil {
		idx.Networks = map[string]*Record{}
	}
	return idx, nil
}

func (s *Store) withLeases(write bool, fn func(*leaseIndex) error) error {
	ctx := context.Background()
	if write {
		return s.leaseEngine.Update(ctx, meta.Scope{Write: "leases"}, meta.CommitDurable, func(writer meta.Writer) error {
			leases, err := s.readLeaseIndex(ctx, writer)
			if err != nil {
				return err
			}
			if err := fn(leases); err != nil {
				return err
			}
			return leaseIndexCollection.Upsert(ctx, writer, networkLeaseRecord, leases)
		})
	}
	return s.leaseEngine.View(ctx, []meta.Namespace{"leases"}, func(reader meta.Reader) error {
		leases, err := s.readLeaseIndex(ctx, reader)
		if err != nil {
			return err
		}
		return fn(leases)
	})
}

func (s *Store) readLeaseIndex(ctx context.Context, reader meta.Reader) (*leaseIndex, error) {
	leases, err := leaseIndexCollection.Get(ctx, reader, networkLeaseRecord)
	if errors.Is(err, meta.ErrNotFound) {
		leases = &leaseIndex{SchemaVersion: leaseSchemaVersion, Leases: map[string]*Lease{}}
	} else if err != nil {
		return nil, fmt.Errorf("read network leases: %w", err)
	}
	if leases.SchemaVersion != "" && leases.SchemaVersion != leaseSchemaVersion {
		return nil, fmt.Errorf("unsupported network leases schema %q", leases.SchemaVersion)
	}
	leases.init()
	return leases, nil
}

func (s *Store) readHostTapState() (*HostTapState, error) {
	var state *HostTapState
	err := s.withHostTap(false, func(current **HostTapState) error {
		state = cloneHostTapState(*current)
		return nil
	})
	return state, err
}

func (s *Store) writeHostTapState(state *HostTapState) error {
	return s.withHostTap(true, func(current **HostTapState) error {
		*current = cloneHostTapState(state)
		return nil
	})
}

func (s *Store) removeHostTapState() error {
	return s.withHostTap(true, func(current **HostTapState) error {
		*current = nil
		return nil
	})
}

func (s *Store) adjustHostTapRef(delta int, requireState bool) error {
	return s.withHostTap(true, func(current **HostTapState) error {
		state := *current
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
		return nil
	})
}

func (s *Store) withHostTap(write bool, fn func(**HostTapState) error) error {
	ctx := context.Background()
	read := func(reader meta.Reader) error {
		state, err := hostTapCollection.Get(ctx, reader, hostTapRecord)
		if errors.Is(err, meta.ErrNotFound) {
			state = nil
		} else if err != nil {
			return fmt.Errorf("read host-tap state: %w", err)
		} else if state.SchemaVersion == "" {
			state.SchemaVersion = hostTapSchemaVersion
		}
		return fn(&state)
	}
	if !write {
		return s.hostTapEngine.View(ctx, []meta.Namespace{"host-tap"}, read)
	}
	return s.hostTapEngine.Update(ctx, meta.Scope{Write: "host-tap"}, meta.CommitDurable, func(writer meta.Writer) error {
		stateFn := func(current *HostTapState, hadState bool) error {
			if current == nil {
				if !hadState {
					return nil
				}
				return hostTapCollection.Delete(ctx, writer, hostTapRecord)
			}
			return hostTapCollection.Upsert(ctx, writer, hostTapRecord, current)
		}
		var state *HostTapState
		hadState := false
		if decoded, err := hostTapCollection.Get(ctx, writer, hostTapRecord); err != nil && !errors.Is(err, meta.ErrNotFound) {
			return err
		} else if err == nil {
			hadState = true
			state = decoded
		}
		if err := fn(&state); err != nil {
			return err
		}
		return stateFn(state, hadState)
	})
}

func cloneHostTapState(state *HostTapState) *HostTapState {
	if state == nil {
		return nil
	}
	cloned := *state
	return &cloned
}
