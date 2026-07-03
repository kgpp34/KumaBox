package vmstore

import (
	"path/filepath"
	"time"
)

type VMState string

const (
	StateCreated VMState = "created"
	StateRunning VMState = "running"
	StateStopped VMState = "stopped"
	StateError   VMState = "error"
)

type ObservedState string

const (
	ObservedStateCreated ObservedState = "CREATED"
	ObservedStateRunning ObservedState = "RUNNING"
	ObservedStateStopped ObservedState = "STOPPED"
	ObservedStateFailed  ObservedState = "FAILED"
	ObservedStateUnknown ObservedState = "UNKNOWN"
)

type Observation struct {
	State     ObservedState `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	CheckedAt time.Time     `json:"checkedAt"`
}

type VMRecord struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Backend        string        `json:"backend"`
	State          VMState       `json:"state"`
	ObservedState  ObservedState `json:"observedState,omitempty"`
	ObservedReason string        `json:"observedReason,omitempty"`
	ObservedAt     *time.Time    `json:"observedAt,omitempty"`
	PID            int           `json:"pid,omitempty"`
	APISocket      string        `json:"apiSocket,omitempty"`
	Error          string        `json:"error,omitempty"`
	RootDisk       string        `json:"rootDisk"`
	Kernel         string        `json:"kernel,omitempty"`
	Initrd         string        `json:"initrd,omitempty"`
	Firmware       string        `json:"firmware,omitempty"`
	Image          *ImageRef     `json:"image,omitempty"`
	Metadata       *Metadata     `json:"metadata,omitempty"`
	RunDir         string        `json:"runDir"`
	LogDir         string        `json:"logDir"`
	Config         string        `json:"config"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
	StartedAt      *time.Time    `json:"startedAt,omitempty"`
	StoppedAt      *time.Time    `json:"stoppedAt,omitempty"`
	FirstBooted    bool          `json:"firstBooted,omitempty"`
}

type Metadata struct {
	Type       string `json:"type"`
	CidataDir  string `json:"cidataDir"`
	CidataDisk string `json:"cidataDisk"`
}

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

func cloneImageRef(ref *ImageRef) *ImageRef {
	if ref == nil {
		return nil
	}
	copied := *ref
	return &copied
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
