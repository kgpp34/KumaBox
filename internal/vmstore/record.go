package vmstore

import (
	"path/filepath"
	"time"
)

type VMState string

const (
	StateCreated VMState = "created"
	StateRunning VMState = "running"
	StateError   VMState = "error"
)

type VMRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Backend   string     `json:"backend"`
	State     VMState    `json:"state"`
	PID       int        `json:"pid,omitempty"`
	APISocket string     `json:"apiSocket,omitempty"`
	Error     string     `json:"error,omitempty"`
	RootDisk  string     `json:"rootDisk"`
	Kernel    string     `json:"kernel,omitempty"`
	Initrd    string     `json:"initrd,omitempty"`
	Firmware  string     `json:"firmware,omitempty"`
	RunDir    string     `json:"runDir"`
	LogDir    string     `json:"logDir"`
	Config    string     `json:"config"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
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

	return &VMRecord{
		ID:        id,
		Name:      req.Name,
		Backend:   backendCloudHypervisor,
		State:     StateCreated,
		RootDisk:  rootDisk,
		Kernel:    kernel,
		Initrd:    initrd,
		Firmware:  firmware,
		RunDir:    runDir,
		LogDir:    logDir,
		Config:    filepath.Join(runDir, "cloud-hypervisor.json"),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func cloneRecord(rec *VMRecord) *VMRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	return &copied
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
