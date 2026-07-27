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

const (
	FormatRaw        = "raw"
	FormatQCOW2      = "qcow2"
	FilesystemEXT4   = "ext4"
	FilesystemEROFS  = "erofs"
	FilesystemNone   = "none"
	StorageIDCOW     = "cow"
	StorageIDCidata  = "cidata"
	StorageSerialCOW = "kumabox-cow"
	BaseFamilyOCI    = "oci"
)

func LayerID(index int) string {
	return fmt.Sprintf("layer%d", index)
}

func LayerSerial(index int) string {
	return fmt.Sprintf("kumabox-layer%d", index)
}

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
	ID                  string                   `json:"id"`
	Name                string                   `json:"name"`
	Backend             string                   `json:"backend"`
	State               VMState                  `json:"state"`
	ObservedState       ObservedState            `json:"observedState,omitempty"`
	ObservedReason      string                   `json:"observedReason,omitempty"`
	ObservedAt          *time.Time               `json:"observedAt,omitempty"`
	PID                 int                      `json:"pid,omitempty"`
	APISocket           string                   `json:"apiSocket,omitempty"`
	VsockSocket         string                   `json:"vsockSocket,omitempty"`
	Error               string                   `json:"error,omitempty"`
	Restore             *RestoreStatus           `json:"restore,omitempty"`
	LastRestore         *RestoreResult           `json:"lastRestore,omitempty"`
	Performance         *PerformanceMetrics      `json:"performance,omitempty"`
	SnapshotDependency  *SnapshotDependency      `json:"snapshotDependency,omitempty"`
	Hibernate           *HibernateStatus         `json:"hibernate,omitempty"`
	RootDisk            string                   `json:"rootDisk"`
	Kernel              string                   `json:"kernel,omitempty"`
	Initrd              string                   `json:"initrd,omitempty"`
	KernelCmdline       string                   `json:"kernelCmdline,omitempty"`
	Firmware            string                   `json:"firmware,omitempty"`
	Image               *ImageRef                `json:"image,omitempty"`
	CPUs                int                      `json:"cpus"`
	MemoryBytes         int64                    `json:"memoryBytes"`
	SharedMemory        bool                     `json:"sharedMemory,omitempty"`
	Metadata            *Metadata                `json:"metadata,omitempty"`
	StorageConfigs      []StorageConfig          `json:"storageConfigs,omitempty"`
	AttachedDisks       []AttachedDisk           `json:"attachedDisks,omitempty"`
	AttachedFilesystems []AttachedFilesystem     `json:"attachedFilesystems,omitempty"`
	NetworkConfigs      []kbnetwork.Config       `json:"networkConfigs,omitempty"`
	Network             string                   `json:"network,omitempty"`
	Networks            []string                 `json:"networks,omitempty"`
	NetworkStatus       *kbnetwork.InspectResult `json:"networkStatus,omitempty"`
	RunDir              string                   `json:"runDir"`
	LogDir              string                   `json:"logDir"`
	Config              string                   `json:"config"`
	CreatedAt           time.Time                `json:"createdAt"`
	UpdatedAt           time.Time                `json:"updatedAt"`
	StartedAt           *time.Time               `json:"startedAt,omitempty"`
	StoppedAt           *time.Time               `json:"stoppedAt,omitempty"`
	FirstBooted         bool                     `json:"firstBooted,omitempty"`
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
	SnapshotID               string    `json:"snapshotId"`
	Mode                     string    `json:"mode"`
	DurationMs               int64     `json:"durationMs"`
	NativeStageDurationMs    int64     `json:"nativeStageDurationMs"`
	DiskStageDurationMs      int64     `json:"diskStageDurationMs"`
	DiskCommitDurationMs     int64     `json:"diskCommitDurationMs"`
	BackendRestoreDurationMs int64     `json:"backendRestoreDurationMs"`
	IdentityDurationMs       int64     `json:"identityDurationMs"`
	ReadinessDurationMs      int64     `json:"readinessDurationMs"`
	CompletedAt              time.Time `json:"completedAt"`
}

// PerformanceMetrics records the user-visible lifecycle milestones for the
// latest create-and-start or start operation. Phase times are wall-clock
// timestamps for inspection; duration fields are calculated from a monotonic
// clock before persistence.
type PerformanceMetrics struct {
	Operation              string     `json:"operation"`
	ImageDigest            string     `json:"imageDigest,omitempty"`
	EnvironmentFingerprint string     `json:"environmentFingerprint,omitempty"`
	CommandStartedAt       time.Time  `json:"commandStartedAt"`
	ImageResolvedAt        *time.Time `json:"imageResolvedAt,omitempty"`
	StorageReadyAt         *time.Time `json:"storageReadyAt,omitempty"`
	NetworkReadyAt         *time.Time `json:"networkReadyAt,omitempty"`
	VMMSpawnedAt           *time.Time `json:"vmmSpawnedAt,omitempty"`
	VMMAPIReadyAt          *time.Time `json:"vmmAPIReadyAt,omitempty"`
	AgentConnectedAt       *time.Time `json:"agentConnectedAt,omitempty"`
	FirstExecCompletedAt   *time.Time `json:"firstExecCompletedAt,omitempty"`
	VMMAPIReadyDurationMs  int64      `json:"vmmAPIReadyDurationMs,omitempty"`
	AgentReadyDurationMs   int64      `json:"agentReadyDurationMs,omitempty"`
	FirstExecDurationMs    int64      `json:"firstExecDurationMs,omitempty"`
	ReadyDurationMs        int64      `json:"readyDurationMs,omitempty"`
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
	DirectIO         *bool        `json:"directIO,omitempty"`
	Format           string       `json:"format,omitempty"`
	Serial           string       `json:"serial,omitempty"`
	Filesystem       string       `json:"filesystem,omitempty"`
	MountPoint       string       `json:"mountPoint,omitempty"`
	VirtualSizeBytes int64        `json:"virtualSizeBytes,omitempty"`
	Base             *StorageBase `json:"base,omitempty"`
	Type             string       `json:"type,omitempty"`      // Legacy P3 field.
	ImageType        string       `json:"imageType,omitempty"` // Legacy P3 field.
	SourceLayer      string       `json:"sourceLayer,omitempty"`
	SizeBytes        int64        `json:"sizeBytes,omitempty"` // Legacy P3 field.
}

// DataDiskRequest describes a managed writable disk created together with a VM.
// The VM record stores the normalized result as a StorageConfig.
type DataDiskRequest struct {
	Name       string
	SizeBytes  int64
	Filesystem string
	MountPoint string
	MountSet   bool
	DirectIO   *bool
}

type AttachedDisk struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

type AttachedFilesystem struct {
	ID     string `json:"id"`
	Tag    string `json:"tag"`
	Socket string `json:"socket"`
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
	dataConfigs, err := normalizeDataDisks(req.DataDisks, rootDir, id)
	if err != nil {
		return nil, err
	}
	storageConfigs = append(storageConfigs, dataConfigs...)
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
		SharedMemory:   req.SharedMemory,
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
	if rec.Performance != nil {
		performance := *rec.Performance
		performance.ImageResolvedAt = cloneTime(rec.Performance.ImageResolvedAt)
		performance.StorageReadyAt = cloneTime(rec.Performance.StorageReadyAt)
		performance.NetworkReadyAt = cloneTime(rec.Performance.NetworkReadyAt)
		performance.VMMSpawnedAt = cloneTime(rec.Performance.VMMSpawnedAt)
		performance.VMMAPIReadyAt = cloneTime(rec.Performance.VMMAPIReadyAt)
		performance.AgentConnectedAt = cloneTime(rec.Performance.AgentConnectedAt)
		performance.FirstExecCompletedAt = cloneTime(rec.Performance.FirstExecCompletedAt)
		copied.Performance = &performance
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
	copied.AttachedDisks = append([]AttachedDisk(nil), rec.AttachedDisks...)
	copied.AttachedFilesystems = append([]AttachedFilesystem(nil), rec.AttachedFilesystems...)
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

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
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
			if cfg.Format == FormatQCOW2 {
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

func normalizeDataDisks(disks []DataDiskRequest, rootDir, vmID string) ([]StorageConfig, error) {
	if len(disks) == 0 {
		return nil, nil
	}
	configs := make([]StorageConfig, 0, len(disks))
	seen := make(map[string]struct{}, len(disks))
	for i, disk := range disks {
		if err := validateDataDiskRequest(disk); err != nil {
			return nil, fmt.Errorf("data disk %d: %w", i, err)
		}
		if _, exists := seen[disk.Name]; exists {
			return nil, fmt.Errorf("duplicate data disk name %q", disk.Name)
		}
		seen[disk.Name] = struct{}{}
		filesystem := disk.Filesystem
		if filesystem == "" {
			filesystem = FilesystemEXT4
		}
		id := "data-" + disk.Name
		mountPoint := disk.MountPoint
		if !disk.MountSet && filesystem != FilesystemNone {
			mountPoint = "/mnt/" + disk.Name
		}
		configs = append(configs, StorageConfig{
			ID: id, Role: StorageRoleData,
			Path:     filepath.Join(rootDir, "storage", "vms", vmID, id+".raw"),
			Readonly: false, DirectIO: disk.DirectIO, Format: FormatRaw,
			Serial: disk.Name, Filesystem: filesystem, MountPoint: mountPoint,
			VirtualSizeBytes: disk.SizeBytes,
		})
	}
	return configs, nil
}

func validateDataDiskRequest(disk DataDiskRequest) error {
	if !validStorageName(disk.Name) {
		return fmt.Errorf("name %q must start with a letter and contain only letters, digits, '_' or '-'", disk.Name)
	}
	if disk.SizeBytes < 16<<20 {
		return fmt.Errorf("size must be at least 16MiB")
	}
	filesystem := disk.Filesystem
	if filesystem == "" {
		filesystem = FilesystemEXT4
	}
	if filesystem != FilesystemEXT4 && filesystem != FilesystemNone {
		return fmt.Errorf("filesystem %q is unsupported", filesystem)
	}
	if disk.MountPoint != "" {
		if !filepath.IsAbs(disk.MountPoint) || disk.MountPoint == "/" || strings.ContainsAny(disk.MountPoint, "\x00\n") {
			return fmt.Errorf("mount point %q must be an absolute non-root path", disk.MountPoint)
		}
	}
	if filesystem == FilesystemNone && disk.MountPoint != "" {
		return fmt.Errorf("mount point requires a filesystem")
	}
	return nil
}

func validStorageName(value string) bool {
	if len(value) == 0 || len(value) > 20 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
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
