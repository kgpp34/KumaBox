package image

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kumabox/kumabox/types"
)

// imageOutput is the CLI JSON schema, keeping serialization separate from domain types.
// Detailed output retains full digests and byte counts; table formatting is presentation only.
type imageOutput struct {
	// Names are all local aliases associated with this manifest.
	Names []string `json:"names"`
	// ManifestDigest is the complete normalized manifest content identity.
	ManifestDigest string `json:"manifest_digest"`
	// Platform selects the Linux guest OS and architecture.
	Platform platformOutput `json:"platform"`
	// Layers preserve source order from the base layer to the topmost layer.
	Layers []layerOutput `json:"layers"`
	// Boot identifies selected kernel and initrd artifacts.
	Boot bootOutput `json:"boot"`
	// Size is the sum of converted EROFS layer sizes in bytes.
	Size int64 `json:"size"`
	// CreatedAt records local import publication time.
	CreatedAt time.Time `json:"created_at"`
}

type (
	// platformOutput exposes the guest platform without domain serialization methods.
	platformOutput struct {
		// OS is the guest operating system.
		OS string `json:"os"`
		// Architecture is the guest CPU architecture.
		Architecture string `json:"architecture"`
	}
	// layerOutput links an original layer to its converted filesystem and boot metadata.
	layerOutput struct {
		// SourceDigest identifies the original source layer blob.
		SourceDigest string `json:"source_digest"`
		// EROFSDigest identifies the converted filesystem artifact.
		EROFSDigest string `json:"erofs_digest"`
		// Size is the converted EROFS artifact size in bytes.
		Size int64 `json:"size"`
		// BootFiles are regular boot candidates extracted from this source layer.
		BootFiles []bootFileOutput `json:"boot_files"`
		// Whiteouts mark boot paths removed by this layer for overlay boot selection.
		Whiteouts []string `json:"whiteouts,omitempty"`
		// BootOpaque hides boot candidates from lower layers under an opaque boot directory.
		BootOpaque bool `json:"boot_opaque,omitempty"`
	}

	// bootFileOutput describes one content-addressed regular boot candidate.
	bootFileOutput struct {
		// Name is the boot candidate filename within the layer.
		Name string `json:"name"`
		// Digest identifies the extracted file contents.
		Digest string `json:"digest"`
		// Size is the extracted file length in bytes.
		Size int64 `json:"size"`
	}
	// bootOutput identifies the selected boot filenames and their source layer identities.
	bootOutput struct {
		// KernelLayer is the source digest of the layer providing the selected kernel.
		KernelLayer string `json:"kernel_layer"`
		// KernelFile is the selected kernel filename.
		KernelFile string `json:"kernel_file"`
		// InitrdLayer is the source digest of the layer providing the selected initrd.
		InitrdLayer string `json:"initrd_layer"`
		// InitrdFile is the selected initrd filename.
		InitrdFile string `json:"initrd_file"`
	}
)

// imageResult projects domain metadata into the CLI schema without truncating content identities.
func imageResult(image types.Image) imageOutput {
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

// writeImage reports the aliases and full manifest digest after a successful import.
func writeImage(writer io.Writer, image types.Image) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\n", strings.Join(image.Names, ","), image.ManifestDigest)
	return err
}

// writeJSON emits indented JSON followed by a newline for readable inspection and piping.
func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// writeImagesTable renders a header even for an empty catalog and aligns readable summaries.
// Full digests remain available through inspect and list --json.
func writeImagesTable(writer io.Writer, items []types.Image) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tIMAGE ID\tPLATFORM\tSIZE\tCREATED"); err != nil {
		return err
	}
	for _, item := range items {
		names := strings.Join(item.Names, ", ")
		if names == "" {
			names = "<none>"
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s/%s\t%s\t%s\n",
			names, item.ManifestDigest.Hex()[:12], item.Platform.OS, item.Platform.Architecture,
			imageSize(item.Size), item.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

// imageSize formats bytes with decimal SI units, carrying values that would round to 1000.0.
func imageSize(size int64) string {
	if size < 1000 {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	for _, unit := range []string{"kB", "MB", "GB", "TB", "PB", "EB"} {
		value /= 1000
		if value < 999.95 || unit == "EB" {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
	}
	return fmt.Sprintf("%dB", size)
}
