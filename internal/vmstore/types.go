package vmstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	BackendCloudHypervisor = "cloud-hypervisor"

	StateCreated = "created"
)

var (
	ErrNotFound     = errors.New("vm not found")
	ErrNameConflict = errors.New("vm name already exists")
	ErrAmbiguous    = errors.New("vm ref is ambiguous")
)

type VMRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Backend   string    `json:"backend"`
	State     string    `json:"state"`
	RootDisk  string    `json:"rootDisk"`
	Kernel    string    `json:"kernel"`
	Initrd    string    `json:"initrd"`
	RunDir    string    `json:"runDir"`
	LogDir    string    `json:"logDir"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type VMIndex struct {
	VMs   map[string]*VMRecord `json:"vms"`
	Names map[string]string    `json:"names"`
}

type CreateRequest struct {
	Name     string
	RootDisk string
	Kernel   string
	Initrd   string
	RunDir   string
	LogDir   string
}

func (idx *VMIndex) Init() {
	if idx.VMs == nil {
		idx.VMs = make(map[string]*VMRecord)
	}
	if idx.Names == nil {
		idx.Names = make(map[string]string)
	}
}

func (idx *VMIndex) Resolve(ref string) (string, error) {
	idx.Init()
	if _, ok := idx.VMs[ref]; ok {
		return ref, nil
	}
	if id, ok := idx.Names[ref]; ok {
		return id, nil
	}
	if len(ref) < 3 {
		return "", ErrNotFound
	}

	var matched string
	for id := range idx.VMs {
		if !strings.HasPrefix(id, ref) {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("%w: %s", ErrAmbiguous, ref)
		}
		matched = id
	}
	if matched == "" {
		return "", ErrNotFound
	}
	return matched, nil
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}
