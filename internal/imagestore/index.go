// SPDX-License-Identifier: MIT

package imagestore

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound is returned when an image reference does not resolve.
	ErrNotFound = errors.New("image not found")
	// ErrNameConflict is returned when an image name already exists.
	ErrNameConflict = errors.New("image name already exists")
	// ErrAmbiguous is returned when an image reference matches multiple images.
	ErrAmbiguous = errors.New("image ref is ambiguous")
	// ErrChecksumMismatch is returned when a pulled image does not match the expected digest.
	ErrChecksumMismatch = errors.New("image checksum mismatch")
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
