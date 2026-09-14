package image

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kumabox/kumabox/images"
)

// CLI serialization belongs to the command adapter, not image domain types.
type imageOutput struct {
	Names          []string       `json:"names"`
	ManifestDigest string         `json:"manifest_digest"`
	Platform       platformOutput `json:"platform"`
	Layers         []layerOutput  `json:"layers"`
	Boot           bootOutput     `json:"boot"`
	Size           int64          `json:"size"`
	CreatedAt      time.Time      `json:"created_at"`
}

type (
	platformOutput struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	layerOutput struct {
		SourceDigest string           `json:"source_digest"`
		EROFSDigest  string           `json:"erofs_digest"`
		Size         int64            `json:"size"`
		BootFiles    []bootFileOutput `json:"boot_files"`
		Whiteouts    []string         `json:"whiteouts,omitempty"`
		BootOpaque   bool             `json:"boot_opaque,omitempty"`
	}

	bootFileOutput struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	bootOutput struct {
		KernelLayer string `json:"kernel_layer"`
		KernelFile  string `json:"kernel_file"`
		InitrdLayer string `json:"initrd_layer"`
		InitrdFile  string `json:"initrd_file"`
	}
)

func imageResult(image images.Image) imageOutput {
	layers := make([]layerOutput, 0, len(image.Layers))
	for _, layer := range image.Layers {
		bootFiles := make([]bootFileOutput, 0, len(layer.BootFiles))
		for _, file := range layer.BootFiles {
			bootFiles = append(bootFiles, bootFileOutput{Name: file.Name, Digest: file.Digest.String(), Size: file.Size})
		}
		layers = append(layers, layerOutput{SourceDigest: layer.SourceDigest.String(), EROFSDigest: layer.EROFSDigest.String(), Size: layer.Size, BootFiles: bootFiles, Whiteouts: layer.Whiteouts, BootOpaque: layer.BootOpaque})
	}
	return imageOutput{Names: image.Names, ManifestDigest: image.ManifestDigest.String(), Platform: platformOutput{OS: image.Platform.OS, Architecture: image.Platform.Architecture}, Layers: layers, Boot: bootOutput{KernelLayer: image.Boot.KernelLayer.String(), KernelFile: image.Boot.KernelFile, InitrdLayer: image.Boot.InitrdLayer.String(), InitrdFile: image.Boot.InitrdFile}, Size: image.Size, CreatedAt: image.CreatedAt}
}

func writeImage(writer io.Writer, image images.Image) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\n", strings.Join(image.Names, ","), image.ManifestDigest)
	return err
}

type textReporter struct {
	writer io.Writer
}

func (r textReporter) Layer(position, total int, digest images.Digest) error {
	_, err := fmt.Fprintf(r.writer, "layer %d/%d %s\n", position+1, total, digest)
	return err
}

func (r textReporter) Committed(images.Image) error { return nil }
