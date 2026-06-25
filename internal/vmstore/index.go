package vmstore

import (
	"errors"
	"fmt"
	"strings"
)

const backendCloudHypervisor = "cloud-hypervisor"

var (
	ErrNotFound     = errors.New("vm not found")
	ErrNameConflict = errors.New("vm name already exists")
	ErrAmbiguous    = errors.New("vm ref is ambiguous")
)

type vmIndex struct {
	VMs   map[string]*VMRecord `json:"vms"`
	Names map[string]string    `json:"names"`
}

func (idx *vmIndex) init() {
	if idx.VMs == nil {
		idx.VMs = make(map[string]*VMRecord)
	}
	if idx.Names == nil {
		idx.Names = make(map[string]string)
	}
}

func (idx *vmIndex) resolve(ref string) (string, error) {
	idx.init()
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
