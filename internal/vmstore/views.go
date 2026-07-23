package vmstore

import (
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

// VMIdentity is the stable identity used to address a VM across restarts.
type VMIdentity struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Backend   string    `json:"backend"`
	CreatedAt time.Time `json:"createdAt"`
}

// VMConfig contains desired VM configuration. It is independent of whether a
// backend process is currently alive.
type VMConfig struct {
	RootDisk      string          `json:"rootDisk"`
	Kernel        string          `json:"kernel,omitempty"`
	Initrd        string          `json:"initrd,omitempty"`
	KernelCmdline string          `json:"kernelCmdline,omitempty"`
	Firmware      string          `json:"firmware,omitempty"`
	Image         *ImageRef       `json:"image,omitempty"`
	CPUs          int             `json:"cpus"`
	MemoryBytes   int64           `json:"memoryBytes"`
	Metadata      *Metadata       `json:"metadata,omitempty"`
	Network       string          `json:"network,omitempty"`
	Networks      []string        `json:"networks,omitempty"`
	Storage       []StorageConfig `json:"storageConfigs,omitempty"`
}

// VMRuntimeState contains observed and operation-sensitive state.
type VMRuntimeState struct {
	Desired            VMState             `json:"desiredState"`
	Observed           ObservedState       `json:"observedState,omitempty"`
	ObservedReason     string              `json:"observedReason,omitempty"`
	ObservedAt         *time.Time          `json:"observedAt,omitempty"`
	PID                int                 `json:"pid,omitempty"`
	APISocket          string              `json:"apiSocket,omitempty"`
	VsockSocket        string              `json:"vsockSocket,omitempty"`
	Error              string              `json:"error,omitempty"`
	Restore            *RestoreStatus      `json:"restore,omitempty"`
	LastRestore        *RestoreResult      `json:"lastRestore,omitempty"`
	Performance        *PerformanceMetrics `json:"performance,omitempty"`
	SnapshotDependency *SnapshotDependency `json:"snapshotDependency,omitempty"`
	Hibernate          *HibernateStatus    `json:"hibernate,omitempty"`
	StartedAt          *time.Time          `json:"startedAt,omitempty"`
	StoppedAt          *time.Time          `json:"stoppedAt,omitempty"`
	FirstBooted        bool                `json:"firstBooted,omitempty"`
}

// VMAttachments contains host resources allocated for this VM.
type VMAttachments struct {
	NetworkConfigs []kbnetwork.Config       `json:"networkConfigs,omitempty"`
	NetworkStatus  *kbnetwork.InspectResult `json:"networkStatus,omitempty"`
	RunDir         string                   `json:"runDir"`
	LogDir         string                   `json:"logDir"`
	Config         string                   `json:"config"`
}

// VMReferences contains durable resource ownership relationships.
type VMReferences struct {
	ImageID     string   `json:"imageId,omitempty"`
	SnapshotIDs []string `json:"snapshotIds,omitempty"`
}

func (r *VMRecord) IdentityView() VMIdentity {
	if r == nil {
		return VMIdentity{}
	}
	return VMIdentity{ID: r.ID, Name: r.Name, Backend: r.Backend, CreatedAt: r.CreatedAt}
}

func (r *VMRecord) ConfigView() VMConfig {
	if r == nil {
		return VMConfig{}
	}
	return VMConfig{RootDisk: r.RootDisk, Kernel: r.Kernel, Initrd: r.Initrd, KernelCmdline: r.KernelCmdline, Firmware: r.Firmware, Image: cloneImageRef(r.Image), CPUs: r.CPUs, MemoryBytes: r.MemoryBytes, Metadata: cloneMetadata(r.Metadata), Network: r.Network, Networks: append([]string(nil), r.Networks...), Storage: cloneStorageConfigs(r.StorageConfigs)}
}

func (r *VMRecord) RuntimeView() VMRuntimeState {
	if r == nil {
		return VMRuntimeState{}
	}
	return VMRuntimeState{Desired: r.State, Observed: r.ObservedState, ObservedReason: r.ObservedReason, ObservedAt: cloneTime(r.ObservedAt), PID: r.PID, APISocket: r.APISocket, VsockSocket: r.VsockSocket, Error: r.Error, Restore: cloneRestoreStatus(r.Restore), LastRestore: cloneRestoreResult(r.LastRestore), Performance: clonePerformance(r.Performance), SnapshotDependency: cloneSnapshotDependency(r.SnapshotDependency), Hibernate: cloneHibernateStatus(r.Hibernate), StartedAt: cloneTime(r.StartedAt), StoppedAt: cloneTime(r.StoppedAt), FirstBooted: r.FirstBooted}
}

func (r *VMRecord) AttachmentsView() VMAttachments {
	if r == nil {
		return VMAttachments{}
	}
	return VMAttachments{NetworkConfigs: cloneNetworkConfigs(r.NetworkConfigs), NetworkStatus: cloneNetworkStatus(r.NetworkStatus), RunDir: r.RunDir, LogDir: r.LogDir, Config: r.Config}
}

func (r *VMRecord) ReferencesView() VMReferences {
	if r == nil {
		return VMReferences{}
	}
	refs := VMReferences{}
	if r.Image != nil {
		refs.ImageID = r.Image.ID
	}
	if r.SnapshotDependency != nil && r.SnapshotDependency.SnapshotID != "" {
		refs.SnapshotIDs = []string{r.SnapshotDependency.SnapshotID}
	}
	if r.Hibernate != nil && r.Hibernate.SnapshotID != "" {
		refs.SnapshotIDs = append(refs.SnapshotIDs, r.Hibernate.SnapshotID)
	}
	return refs
}

func cloneMetadata(value *Metadata) *Metadata {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneRestoreStatus(value *RestoreStatus) *RestoreStatus {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneRestoreResult(value *RestoreResult) *RestoreResult {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func clonePerformance(value *PerformanceMetrics) *PerformanceMetrics {
	if value == nil {
		return nil
	}
	copied := *value
	copied.ImageResolvedAt = cloneTime(value.ImageResolvedAt)
	copied.StorageReadyAt = cloneTime(value.StorageReadyAt)
	copied.NetworkReadyAt = cloneTime(value.NetworkReadyAt)
	copied.VMMSpawnedAt = cloneTime(value.VMMSpawnedAt)
	copied.VMMAPIReadyAt = cloneTime(value.VMMAPIReadyAt)
	copied.AgentConnectedAt = cloneTime(value.AgentConnectedAt)
	copied.FirstExecCompletedAt = cloneTime(value.FirstExecCompletedAt)
	return &copied
}

func cloneSnapshotDependency(value *SnapshotDependency) *SnapshotDependency {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneHibernateStatus(value *HibernateStatus) *HibernateStatus {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
