package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type createVMFlags struct {
	name     string
	rootDisk string
	kernel   string
	initrd   string
	firmware string
	cpus     int
	memory   string
	storage  string
	networks []string
}

func addCreateVMFlags(cmd *cobra.Command, flags *createVMFlags) {
	cmd.Flags().StringVar(&flags.name, "name", "", "VM name")
	cmd.Flags().StringVar(&flags.rootDisk, "root-disk", "", "root disk path")
	cmd.Flags().StringVar(&flags.kernel, "kernel", "", "kernel image path")
	cmd.Flags().StringVar(&flags.initrd, "initrd", "", "initrd image path")
	cmd.Flags().StringVar(&flags.firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().IntVar(&flags.cpus, "cpus", 1, "number of vCPUs")
	cmd.Flags().StringVar(&flags.memory, "memory", "512M", "guest memory size, for example 512M or 2G")
	cmd.Flags().StringVar(&flags.storage, "storage", "", "per-VM writable COW size for OCI images, for example 4G")
	cmd.Flags().StringArrayVar(&flags.networks, "network", nil, "network attachment, repeatable: none, default, host-tap, cni, or cni:NAME")
	_ = cmd.MarkFlagRequired("name")
}

func newCreateRequest(flags createVMFlags, args []string, cfg config.Config) (vmstore.CreateRequest, error) {
	if flags.cpus < 0 {
		return vmstore.CreateRequest{}, fmt.Errorf("--cpus must be greater than zero")
	}
	if flags.cpus == 0 {
		flags.cpus = 1
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, "512M"))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	if len(args) == 0 {
		if flags.rootDisk == "" {
			return vmstore.CreateRequest{}, fmt.Errorf("either IMAGE or --root-disk is required")
		}
		return vmstore.CreateRequest{Name: flags.name, RootDisk: flags.rootDisk, Kernel: flags.kernel, Initrd: flags.initrd, Firmware: flags.firmware, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedNetworkFlags(flags.networks), RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}, nil
	}
	if flags.rootDisk != "" || flags.kernel != "" || flags.initrd != "" || flags.firmware != "" {
		return vmstore.CreateRequest{}, fmt.Errorf("IMAGE cannot be combined with --root-disk, --kernel, --initrd, or --firmware")
	}
	stores, err := configuredStores(cfg)
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	image, err := stores.Images.Inspect(args[0])
	if err != nil {
		return vmstore.CreateRequest{}, fmt.Errorf("resolve image %q: %w", args[0], err)
	}
	if image.OCI == nil && image.RootDisk.Path == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no root disk", args[0])
	}
	if image.OCI != nil {
		return newOCIImageCreateRequest(flags, image, cfg)
	}
	if image.RootDisk.Format != vmstore.FormatQCOW2 {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q root disk format %q cannot use a qcow2 overlay", image.Name, image.RootDisk.Format)
	}
	if image.RootDisk.SHA256 == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q root disk has no pinned sha256 digest", image.Name)
	}
	digest := image.RootDisk.SHA256
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	req := vmstore.CreateRequest{Name: flags.name, RootDisk: image.RootDisk.Path, Kernel: image.Boot.Kernel, Initrd: image.Boot.Initrd, Firmware: image.Boot.Firmware, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedNetworkFlags(flags.networks), Image: &vmstore.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: digest}, StorageConfigs: []vmstore.StorageConfig{{ID: "root", Role: vmstore.StorageRoleCOW, Format: vmstore.FormatQCOW2, VirtualSizeBytes: image.RootDisk.VirtualSizeBytes, Base: &vmstore.StorageBase{Family: "cloudimg", ImageID: image.ID, Digest: digest, Format: image.RootDisk.Format, Path: image.RootDisk.Path}}}, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}
	if req.Firmware == "" && (req.Kernel == "" || req.Initrd == "") {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no usable boot configuration", args[0])
	}
	return req, nil
}

func newOCIImageCreateRequest(flags createVMFlags, image *imagestore.ImageRecord, cfg config.Config) (vmstore.CreateRequest, error) {
	if image.Boot.Mode != "direct" || image.Boot.Kernel == "" || image.Boot.Initrd == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no OCI direct boot profile", image.Name)
	}
	cowSize, err := parseByteSize(defaultString(flags.storage, defaultOCIStorageSize))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, defaultOCIMemorySize))
	if err != nil {
		return vmstore.CreateRequest{}, err
	}
	manifestDigest := image.OCI.DigestRef
	if _, digest, found := strings.Cut(manifestDigest, "@"); found {
		manifestDigest = digest
	}
	if manifestDigest == "" {
		return vmstore.CreateRequest{}, fmt.Errorf("image %q has no OCI manifest digest", image.Name)
	}
	storageConfigs := make([]vmstore.StorageConfig, 0, len(image.OCI.Layers)+1)
	layerDigests := make([]string, 0, len(image.OCI.Layers))
	for i, layer := range image.OCI.Layers {
		if layer.EROFS == nil || layer.EROFS.Path == "" {
			return vmstore.CreateRequest{}, fmt.Errorf("image %q layer %d has no EROFS blob", image.Name, i)
		}
		serial := layer.Serial
		if serial == "" {
			serial = vmstore.LayerSerial(i)
		}
		storageConfigs = append(storageConfigs, vmstore.StorageConfig{ID: vmstore.LayerID(i), Role: vmstore.StorageRoleLayer, Path: layer.EROFS.Path, Readonly: true, Format: vmstore.FormatRaw, Serial: serial, Filesystem: vmstore.FilesystemEROFS, SourceLayer: layer.Digest, VirtualSizeBytes: layer.EROFS.SizeBytes})
		layerDigests = append(layerDigests, layer.Digest)
	}
	storageConfigs = append(storageConfigs, vmstore.StorageConfig{ID: vmstore.StorageIDCOW, Role: vmstore.StorageRoleCOW, Format: vmstore.FormatRaw, Serial: vmstore.StorageSerialCOW, Filesystem: vmstore.FilesystemEXT4, VirtualSizeBytes: cowSize, Base: &vmstore.StorageBase{Family: vmstore.BaseFamilyOCI, ImageID: image.ID, Digest: manifestDigest, LayerDigests: append([]string(nil), layerDigests...)}})
	return vmstore.CreateRequest{Name: flags.name, Kernel: image.Boot.Kernel, Initrd: image.Boot.Initrd, KernelCmdline: image.Boot.Cmdline, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedOCIImageNetworkFlags(flags.networks, cfg), StorageConfigs: storageConfigs, Image: &vmstore.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: manifestDigest, LayerDigests: append([]string(nil), layerDigests...)}, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}, nil
}
