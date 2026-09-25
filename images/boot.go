package images

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// IsBootName accepts kernel or initrd basenames and excludes .old backups.
// It classifies names only; callers must independently require regular files.
func IsBootName(name string) bool {
	return filepath.Base(name) == name && !strings.HasSuffix(name, ".old") &&
		(strings.HasPrefix(name, "vmlinuz") || strings.HasPrefix(name, "initrd.img"))
}

// SelectBoot applies layer overwrites and whiteouts to regular boot candidates.
// Layers must be ordered from base to top. For each artifact kind the last
// surviving candidate wins; a missing regular kernel or initrd is incompatible.
//
//	base candidates -> opaque reset -> named whiteouts -> current regular files
//	                       (repeat for each layer)                  |
//	                                                               v
//	                                         last kernel + last initrd
func SelectBoot(layers []types.Layer) (types.Boot, error) {
	// candidate retains provenance while upper layers overwrite the visible boot set.
	type candidate struct {
		// layer keys the managed boot directory for this surviving candidate.
		layer types.Digest
		// file supplies the basename and integrity facts selected for boot.
		file types.BootFile
	}
	var candidates []candidate
	for _, layer := range layers {
		if layer.BootOpaque {
			candidates = nil
		}
		for _, name := range layer.Whiteouts {
			var kept []candidate
			for _, c := range candidates {
				if c.file.Name != name {
					kept = append(kept, c)
				}
			}
			candidates = kept
		}
		for _, file := range layer.BootFiles {
			var kept []candidate
			for _, c := range candidates {
				if c.file.Name != file.Name {
					kept = append(kept, c)
				}
			}
			kept = append(kept, candidate{layer: layer.SourceDigest, file: file})
			candidates = kept
		}
	}
	var boot types.Boot
	for _, c := range candidates {
		if strings.HasPrefix(c.file.Name, "vmlinuz") {
			boot.KernelLayer, boot.KernelFile = c.layer, c.file.Name
		}
		if strings.HasPrefix(c.file.Name, "initrd.img") {
			boot.InitrdLayer, boot.InitrdFile = c.layer, c.file.Name
		}
	}
	if boot.KernelLayer.IsZero() {
		return types.Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/vmlinuz* kernel"))
	}
	if boot.InitrdLayer.IsZero() {
		return types.Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/initrd.img* initrd"))
	}
	return boot, nil
}
