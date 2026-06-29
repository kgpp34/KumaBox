package imagestore

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound     = errors.New("image not found")
	ErrNameConflict = errors.New("image name already exists")
	ErrAmbiguous    = errors.New("image ref is ambiguous")
)

type imageIndex struct {
	SchemaVersion string                  `json:"schemaVersion"`
	Images        map[string]*ImageRecord `json:"images"`
	Names         map[string]string       `json:"names"`
}

func (idx *imageIndex) init() {
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = "kumabox.image.index.v1"
	}
	if idx.Images == nil {
		idx.Images = make(map[string]*ImageRecord)
	}
	if idx.Names == nil {
		idx.Names = make(map[string]string)
	}
}

func (idx *imageIndex) resolve(ref string) (string, error) {
	idx.init()
	if _, ok := idx.Images[ref]; ok {
		return ref, nil
	}
	if id, ok := idx.Names[ref]; ok {
		return id, nil
	}
	if len(ref) < 3 {
		return "", ErrNotFound
	}

	var matched string
	for id := range idx.Images {
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
