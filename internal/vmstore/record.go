// Package vmstore persists KumaBox VM intent.
//
// A VM record stores what KumaBox wants to run: disks, boot mode, network
// attachments, and managed directories. Runtime reconciliation augments that
// intent with observed state from the backend, but the store itself does not
// talk to Cloud Hypervisor or the host network.
package vmstore

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

// VMState is KumaBox's persisted lifecycle state.
//
// It is updated by lifecycle operations such as start, stop, and delete. It is
// not a direct probe of the VMM process; callers should compare it with
// ObservedState when reconciling stale records.
type VMState string

const (
	StateCreated VMState = "created"
	StateRunning VMState = "running"
	StateStopped VMState = "stopped"
	StateError   VMState = "error"
)

// ObservedState is the runtime state observed from the backend.
//
// Observed state may diverge from VMState when a daemonless command exits, the
// VMM crashes, or host resources disappear. KumaBox records this separately so
// CLI output can show both desired/persisted state and current reality.
type ObservedState string

const (
	ObservedStateCreated ObservedState = "CREATED"
	ObservedStateRunning ObservedState = "RUNNING"
	ObservedStateStopped ObservedState = "STOPPED"
	ObservedStateFailed  ObservedState = "FAILED"
	ObservedStateUnknown ObservedState = "UNKNOWN"
)

// Observation captures one backend reconciliation result.
//
// Observations are transient values returned by backend probes. Runtime may
// copy the latest observation into VMRecord fields and append lifecycle events
// to the VM log directory.
type Observation struct {
	State     ObservedState `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	CheckedAt time.Time     `json:"checkedAt"`
}

// VMRecord is the durable VM metadata stored in the backend index.
//
// The record intentionally keeps VM identity, boot configuration, network
// attachment intent, and managed paths in one document. Provider-specific
// indexes, such as host-tap leases, remain outside the VM index and are linked
// by NetworkConfigs.
type VMRecord struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	Backend        string                   `json:"backend"`
	State          VMState                  `json:"state"`
	ObservedState  ObservedState            `json:"observedState,omitempty"`
	ObservedReason string                   `json:"observedReason,omitempty"`
	ObservedAt     *time.Time               `json:"observedAt,omitempty"`
	PID            int                      `json:"pid,omitempty"`
	APISocket      string                   `json:"apiSocket,omitempty"`
	Error          string                   `json:"error,omitempty"`
	RootDisk       string                   `json:"rootDisk"`
	Kernel         string                   `json:"kernel,omitempty"`
	Initrd         string                   `json:"initrd,omitempty"`
	Firmware       string                   `json:"firmware,omitempty"`
	Image          *ImageRef                `json:"image,omitempty"`
	CPUs           int                      `json:"cpus"`
	Metadata       *Metadata                `json:"metadata,omitempty"`
	NetworkConfigs []kbnetwork.Config       `json:"networkConfigs,omitempty"`
	Network        string                   `json:"network,omitempty"`
	Networks       []string                 `json:"networks,omitempty"`
	NetworkStatus  *kbnetwork.InspectResult `json:"networkStatus,omitempty"`
	RunDir         string                   `json:"runDir"`
	LogDir         string                   `json:"logDir"`
	Config         string                   `json:"config"`
	CreatedAt      time.Time                `json:"createdAt"`
	UpdatedAt      time.Time                `json:"updatedAt"`
	StartedAt      *time.Time               `json:"startedAt,omitempty"`
	StoppedAt      *time.Time               `json:"stoppedAt,omitempty"`
	FirstBooted    bool                     `json:"firstBooted,omitempty"`
}

// Metadata describes the generated cloud-init NoCloud seed attached to a VM.
//
// Firmware/cloud-image boots use this seed for hostname, user-data, and static
// network configuration. Direct kernel/initrd boots may not need metadata.
type Metadata struct {
	Type       string `json:"type"`
	CidataDir  string `json:"cidataDir"`
	CidataDisk string `json:"cidataDisk"`
}

// ImageRef records the managed image used to create or run a VM.
//
// The root disk path is copied into the VM record so lifecycle operations do
// not need to resolve mutable image names after creation.
type ImageRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RootDisk string `json:"rootDisk"`
	BootMode string `json:"bootMode,omitempty"`
}

func newRecord(id string, req CreateRequest, now time.Time) (*VMRecord, error) {
	rootDisk, err := normalizePath(req.RootDisk)
	if err != nil {
		return nil, err
	}
	kernel, err := normalizePath(req.Kernel)
	if err != nil {
		return nil, err
	}
	initrd, err := normalizePath(req.Initrd)
	if err != nil {
		return nil, err
	}
	firmware, err := normalizePath(req.Firmware)
	if err != nil {
		return nil, err
	}
	runDir, err := normalizePath(filepath.Join(req.RunDir, "vms", id))
	if err != nil {
		return nil, err
	}
	logDir, err := normalizePath(filepath.Join(req.LogDir, "vms", id))
	if err != nil {
		return nil, err
	}

	networks, err := normalizeNetworks(req.Network, req.Networks)
	if err != nil {
		return nil, err
	}
	network := primaryNetwork(networks)
	cpus := normalizeCPUs(req.CPUs)
	rec := &VMRecord{
		ID:        id,
		Name:      req.Name,
		Backend:   backendCloudHypervisor,
		State:     StateCreated,
		RootDisk:  rootDisk,
		Kernel:    kernel,
		Initrd:    initrd,
		Firmware:  firmware,
		Image:     cloneImageRef(req.Image),
		CPUs:      cpus,
		Network:   network,
		Networks:  cloneStrings(networks),
		RunDir:    runDir,
		LogDir:    logDir,
		Config:    filepath.Join(runDir, "cloud-hypervisor.json"),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if firmware != "" {
		rec.Metadata = &Metadata{
			Type:       "nocloud",
			CidataDir:  filepath.Join(runDir, "cidata"),
			CidataDisk: filepath.Join(runDir, "cidata.img"),
		}
	}
	return rec, nil
}

func normalizeCPUs(cpus int) int {
	if cpus <= 0 {
		return 1
	}
	return cpus
}

func cloneRecord(rec *VMRecord) *VMRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	if rec.ObservedAt != nil {
		observedAt := *rec.ObservedAt
		copied.ObservedAt = &observedAt
	}
	if rec.Metadata != nil {
		metadata := *rec.Metadata
		copied.Metadata = &metadata
	}
	copied.Image = cloneImageRef(rec.Image)
	copied.Networks = cloneStrings(rec.Networks)
	copied.NetworkConfigs = cloneNetworkConfigs(rec.NetworkConfigs)
	copied.NetworkStatus = cloneNetworkStatus(rec.NetworkStatus)
	if rec.StartedAt != nil {
		startedAt := *rec.StartedAt
		copied.StartedAt = &startedAt
	}
	if rec.StoppedAt != nil {
		stoppedAt := *rec.StoppedAt
		copied.StoppedAt = &stoppedAt
	}
	return &copied
}

func cloneNetworkStatus(status *kbnetwork.InspectResult) *kbnetwork.InspectResult {
	if status == nil {
		return nil
	}
	copied := *status
	copied.Interfaces = append([]kbnetwork.Record(nil), status.Interfaces...)
	copied.VMConfigs = cloneNetworkConfigs(status.VMConfigs)
	copied.Drift = append([]string(nil), status.Drift...)
	return &copied
}

func cloneNetworkConfigs(configs []kbnetwork.Config) []kbnetwork.Config {
	if len(configs) == 0 {
		return nil
	}
	copied := make([]kbnetwork.Config, len(configs))
	copy(copied, configs)
	for i := range copied {
		if configs[i].Network != nil {
			network := *configs[i].Network
			network.DNS = append([]string(nil), configs[i].Network.DNS...)
			copied[i].Network = &network
		}
	}
	return copied
}

func cloneImageRef(ref *ImageRef) *ImageRef {
	if ref == nil {
		return nil
	}
	copied := *ref
	return &copied
}

func normalizeNetworks(network string, networks []string) ([]string, error) {
	values := append([]string(nil), networks...)
	if len(values) == 0 && network != "" {
		values = append(values, network)
	}
	if len(values) == 0 {
		values = append(values, "none")
	}
	for i, value := range values {
		if value == "" {
			return nil, errors.New("network value must not be empty")
		}
		values[i] = value
	}
	if len(values) > 1 {
		var family string
		for _, value := range values {
			if value == kbnetwork.ProviderNone {
				return nil, errors.New("network none cannot be combined with other networks")
			}
			currentFamily := networkProviderFamily(value)
			if family == "" {
				family = currentFamily
				continue
			}
			if currentFamily != family {
				return nil, errors.New("multiple networks must use the same provider family")
			}
		}
	}
	return values, nil
}

func networkProviderFamily(network string) string {
	if kbnetwork.IsCNISelection(network) {
		return kbnetwork.ProviderCNI
	}
	if network == "default" || network == kbnetwork.ProviderHostTap {
		return kbnetwork.ProviderHostTap
	}
	if strings.HasPrefix(network, kbnetwork.ProviderHostTap+":") {
		return kbnetwork.ProviderHostTap
	}
	return network
}

func primaryNetwork(networks []string) string {
	if len(networks) == 0 {
		return ""
	}
	if len(networks) == 1 {
		return networks[0]
	}
	return "multi"
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
