package vmstore

import (
	"fmt"
	"strings"
)

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
