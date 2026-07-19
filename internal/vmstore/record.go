// Package vmstore persists KumaBox VM intent.
//
// A VM record stores what KumaBox wants to run: disks, boot mode, network
// attachments, and managed directories. Runtime reconciliation augments that
// intent with observed state from the backend, but the store itself does not
// talk to Cloud Hypervisor or the host network.
package vmstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

const defaultMemoryBytes int64 = 512 << 20

// VMState is KumaBox's persisted lifecycle state.
//
// It is updated by lifecycle operations such as start, stop, and delete. It is
// not a direct probe of the VMM process; callers should compare it with
// ObservedState when reconciling stale records.
type VMState string

const (
	StateCreated VMState = "created"
	StateRunning VMState = "running"
	StatePaused  VMState = "paused"
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
	ObservedStatePaused  ObservedState = "PAUSED"
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
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	Backend            string                   `json:"backend"`
	State              VMState                  `json:"state"`
	ObservedState      ObservedState            `json:"observedState,omitempty"`
	ObservedReason     string                   `json:"observedReason,omitempty"`
	ObservedAt         *time.Time               `json:"observedAt,omitempty"`
	PID                int                      `json:"pid,omitempty"`
	APISocket          string                   `json:"apiSocket,omitempty"`
	VsockSocket        string                   `json:"vsockSocket,omitempty"`
	Error              string                   `json:"error,omitempty"`
	Restore            *RestoreStatus           `json:"restore,omitempty"`
	LastRestore        *RestoreResult           `json:"lastRestore,omitempty"`
	SnapshotDependency *SnapshotDependency      `json:"snapshotDependency,omitempty"`
	Hibernate          *HibernateStatus         `json:"hibernate,omitempty"`
	RootDisk           string                   `json:"rootDisk"`
	Kernel             string                   `json:"kernel,omitempty"`
	Initrd             string                   `json:"initrd,omitempty"`
	KernelCmdline      string                   `json:"kernelCmdline,omitempty"`
	Firmware           string                   `json:"firmware,omitempty"`
	Image              *ImageRef                `json:"image,omitempty"`
	CPUs               int                      `json:"cpus"`
	MemoryBytes        int64                    `json:"memoryBytes"`
	Metadata           *Metadata                `json:"metadata,omitempty"`
	StorageConfigs     []StorageConfig          `json:"storageConfigs,omitempty"`
	NetworkConfigs     []kbnetwork.Config       `json:"networkConfigs,omitempty"`
	Network            string                   `json:"network,omitempty"`
	Networks           []string                 `json:"networks,omitempty"`
	NetworkStatus      *kbnetwork.InspectResult `json:"networkStatus,omitempty"`
	RunDir             string                   `json:"runDir"`
	LogDir             string                   `json:"logDir"`
	Config             string                   `json:"config"`
	CreatedAt          time.Time                `json:"createdAt"`
	UpdatedAt          time.Time                `json:"updatedAt"`
	StartedAt          *time.Time               `json:"startedAt,omitempty"`
	StoppedAt          *time.Time               `json:"stoppedAt,omitempty"`
	FirstBooted        bool                     `json:"firstBooted,omitempty"`
}

// RestoreStatus is the durable recovery marker for an in-place native
// restore. Its presence means writable state may have been replaced and a
// normal cold start must fail closed until restore succeeds or the VM is
// deleted.
type RestoreStatus struct {
	SnapshotID string    `json:"snapshotId"`
	Mode       string    `json:"mode"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// RestoreResult records the latest completed native restore for operational
// latency inspection without retaining the transient dirty marker.
type RestoreResult struct {
	SnapshotID  string    `json:"snapshotId"`
	Mode        string    `json:"mode"`
	DurationMs  int64     `json:"durationMs"`
	CompletedAt time.Time `json:"completedAt"`
}

// SnapshotDependency pins native memory payload while a delayed restore mode
// may still fault pages from the source snapshot.
type SnapshotDependency struct {
	SnapshotID string    `json:"snapshotId"`
	Mode       string    `json:"mode"`
	Since      time.Time `json:"since"`
}

// HibernateStatus prevents a cold start from discarding a resumable native
// memory state. Restore clears it only after the VM has resumed successfully.
type HibernateStatus struct {
	SnapshotID string    `json:"snapshotId"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (r *VMRecord) EffectiveMemoryBytes() int64 {
	if r == nil || r.MemoryBytes <= 0 {
		return defaultMemoryBytes
	}
	return r.MemoryBytes
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
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	RootDisk     string   `json:"rootDisk"`
	BootMode     string   `json:"bootMode,omitempty"`
	Digest       string   `json:"digest,omitempty"`
	LayerDigests []string `json:"layerDigests,omitempty"`
}

// StorageRole describes the semantic purpose of a VM block device.
type StorageRole string

const (
	StorageRoleLayer  StorageRole = "layer"
	StorageRoleBase   StorageRole = "base"
	StorageRoleCOW    StorageRole = "cow"
	StorageRoleData   StorageRole = "data"
	StorageRoleCidata StorageRole = "cidata"
)

// StorageBase pins the immutable image assets backing a writable root disk.
// Paths are local resolution hints; digests are the portable identity.
type StorageBase struct {
	Family       string   `json:"family"`
	ImageID      string   `json:"imageId,omitempty"`
	Digest       string   `json:"digest,omitempty"`
	Format       string   `json:"format,omitempty"`
	Path         string   `json:"path,omitempty"`
	LayerDigests []string `json:"layerDigests,omitempty"`
}

// StorageConfig describes one block device owned or referenced by a VM.
type StorageConfig struct {
	ID               string       `json:"id"`
	Role             StorageRole  `json:"role,omitempty"`
	Path             string       `json:"path"`
	Readonly         bool         `json:"readonly"`
	Format           string       `json:"format,omitempty"`
	Serial           string       `json:"serial,omitempty"`
	Filesystem       string       `json:"filesystem,omitempty"`
	VirtualSizeBytes int64        `json:"virtualSizeBytes,omitempty"`
	Base             *StorageBase `json:"base,omitempty"`
	Type             string       `json:"type,omitempty"`      // Legacy P3 field.
	ImageType        string       `json:"imageType,omitempty"` // Legacy P3 field.
	SourceLayer      string       `json:"sourceLayer,omitempty"`
	SizeBytes        int64        `json:"sizeBytes,omitempty"` // Legacy P3 field.
}

// EffectiveRole returns Role or its legacy Type equivalent.
func (c StorageConfig) EffectiveRole() StorageRole {
	if c.Role != "" {
		return c.Role
	}
	return StorageRole(c.Type)
}

// EffectiveFormat returns Format or its legacy ImageType equivalent.
func (c StorageConfig) EffectiveFormat() string {
	if c.Format != "" {
		return c.Format
	}
	return c.ImageType
}

// EffectiveVirtualSize returns VirtualSizeBytes or its legacy SizeBytes value.
func (c StorageConfig) EffectiveVirtualSize() int64 {
	if c.VirtualSizeBytes > 0 {
		return c.VirtualSizeBytes
	}
	return c.SizeBytes
}

func newRecord(id string, req CreateRequest, rootDir string, now time.Time) (*VMRecord, error) {
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
	storageConfigs := normalizeStorageConfigs(req.StorageConfigs, rootDir, id)
	if overlay := cloudImageRootOverlay(storageConfigs); overlay != "" {
		rootDisk = overlay
	}
	rec := &VMRecord{
		ID:             id,
		Name:           req.Name,
		Backend:        backendCloudHypervisor,
		State:          StateCreated,
		RootDisk:       rootDisk,
		Kernel:         kernel,
		Initrd:         initrd,
		KernelCmdline:  req.KernelCmdline,
		Firmware:       firmware,
		Image:          cloneImageRef(req.Image),
		CPUs:           cpus,
		MemoryBytes:    normalizeMemoryBytes(req.MemoryBytes),
		StorageConfigs: storageConfigs,
		Network:        network,
		Networks:       cloneStrings(networks),
		RunDir:         runDir,
		LogDir:         logDir,
		Config:         filepath.Join(runDir, "cloud-hypervisor.json"),
		VsockSocket:    filepath.Join(runDir, "vsock.uds"),
		CreatedAt:      now,
		UpdatedAt:      now,
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

func normalizeMemoryBytes(memoryBytes int64) int64 {
	if memoryBytes <= 0 {
		return defaultMemoryBytes
	}
	return memoryBytes
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
	if rec.Restore != nil {
		restore := *rec.Restore
		copied.Restore = &restore
	}
	if rec.LastRestore != nil {
		lastRestore := *rec.LastRestore
		copied.LastRestore = &lastRestore
	}
	if rec.SnapshotDependency != nil {
		dependency := *rec.SnapshotDependency
		copied.SnapshotDependency = &dependency
	}
	if rec.Hibernate != nil {
		hibernate := *rec.Hibernate
		copied.Hibernate = &hibernate
	}
	copied.Image = cloneImageRef(rec.Image)
	copied.StorageConfigs = cloneStorageConfigs(rec.StorageConfigs)
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

func normalizeStorageConfigs(configs []StorageConfig, rootDir, vmID string) []StorageConfig {
	if len(configs) == 0 {
		return nil
	}
	normalized := make([]StorageConfig, 0, len(configs))
	for i, cfg := range configs {
		if cfg.Role == "" {
			cfg.Role = StorageRole(cfg.Type)
		}
		if cfg.Format == "" {
			cfg.Format = cfg.ImageType
		}
		if cfg.VirtualSizeBytes == 0 {
			cfg.VirtualSizeBytes = cfg.SizeBytes
		}
		cfg.Type = ""
		cfg.ImageType = ""
		cfg.SizeBytes = 0
		if cfg.ID == "" {
			cfg.ID = fmt.Sprintf("storage%d", i)
		}
		if cfg.Role == StorageRoleCOW && cfg.Path == "" {
			name := "cow.ext4"
			if cfg.Base != nil && cfg.Base.Family == "cloudimg" {
				name = "root.overlay.qcow2"
			}
			cfg.Path = filepath.Join(rootDir, "storage", "vms", vmID, name)
		}
		if cfg.Role == StorageRoleData && cfg.Path == "" {
			ext := ".raw"
			if cfg.Format == "qcow2" {
				ext = ".qcow2"
			}
			cfg.Path = filepath.Join(rootDir, "storage", "vms", vmID, "data-"+cfg.ID+ext)
		}
		if abs, err := normalizePath(cfg.Path); err == nil {
			cfg.Path = abs
		}
		normalized = append(normalized, cfg)
	}
	return normalized
}

func cloudImageRootOverlay(configs []StorageConfig) string {
	for _, cfg := range configs {
		if cfg.EffectiveRole() == StorageRoleCOW && cfg.Base != nil && cfg.Base.Family == "cloudimg" {
			return cfg.Path
		}
	}
	return ""
}

func cloneStorageConfigs(configs []StorageConfig) []StorageConfig {
	if len(configs) == 0 {
		return nil
	}
	copied := append([]StorageConfig(nil), configs...)
	for i := range copied {
		if configs[i].Base == nil {
			continue
		}
		base := *configs[i].Base
		base.LayerDigests = cloneStrings(configs[i].Base.LayerDigests)
		copied[i].Base = &base
	}
	return copied
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
	copied.LayerDigests = cloneStrings(ref.LayerDigests)
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
