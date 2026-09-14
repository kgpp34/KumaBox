package images

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
)

func IsBootName(name string) bool {
	return filepath.Base(name) == name && !strings.HasSuffix(name, ".old") &&
		(strings.HasPrefix(name, "vmlinuz") || strings.HasPrefix(name, "initrd.img"))
}

// SelectBoot applies layer overwrites and whiteouts to regular boot candidates.
func SelectBoot(layers []Layer) (Boot, error) {
	type candidate struct {
		layer Digest
		file  BootFile
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
	var boot Boot
	for _, c := range candidates {
		if strings.HasPrefix(c.file.Name, "vmlinuz") {
			boot.KernelLayer, boot.KernelFile = c.layer, c.file.Name
		}
		if strings.HasPrefix(c.file.Name, "initrd.img") {
			boot.InitrdLayer, boot.InitrdFile = c.layer, c.file.Name
		}
	}
	if boot.KernelLayer.IsZero() {
		return Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/vmlinuz* kernel"))
	}
	if boot.InitrdLayer.IsZero() {
		return Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/initrd.img* initrd"))
	}
	return boot, nil
}
