package vmstore

import (
	"path/filepath"
	"time"
)

type VMState string

const StateCreated VMState = "created"

type VMRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Backend   string    `json:"backend"`
	State     VMState   `json:"state"`
	RootDisk  string    `json:"rootDisk"`
	Kernel    string    `json:"kernel"`
	Initrd    string    `json:"initrd"`
	RunDir    string    `json:"runDir"`
	LogDir    string    `json:"logDir"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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

	return &VMRecord{
		ID:        id,
		Name:      req.Name,
		Backend:   backendCloudHypervisor,
		State:     StateCreated,
		RootDisk:  rootDisk,
		Kernel:    kernel,
		Initrd:    initrd,
		RunDir:    filepath.Join(req.RunDir, "vms", id),
		LogDir:    filepath.Join(req.LogDir, "vms", id),
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
