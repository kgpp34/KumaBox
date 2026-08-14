package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/vm"
)

type createVMFlags struct {
	name         string
	rootDisk     string
	kernel       string
	initrd       string
	firmware     string
	cpus         int
	memory       string
	storage      string
	dataDisks    []string
	sharedMemory bool
	networks     []string
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
	cmd.Flags().StringArrayVar(&flags.dataDisks, "data-disk", nil, "managed data disk: size=20G,name=workspace,fstype=ext4,mount=/workspace")
	cmd.Flags().BoolVar(&flags.sharedMemory, "shared-memory", false, "enable shared guest memory for virtio-fs")
	cmd.Flags().StringArrayVar(&flags.networks, "network", nil, "network attachment, repeatable: none, default, host-tap, cni, or cni:NAME")
	_ = cmd.MarkFlagRequired("name")
}

func newCreateRequest(flags createVMFlags, args []string, cfg config.Config) (vm.CreateRequest, error) {
	if flags.cpus < 0 {
		return vm.CreateRequest{}, fmt.Errorf("--cpus must be greater than zero")
	}
	if flags.cpus == 0 {
		flags.cpus = 1
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, "512M"))
	if err != nil {
		return vm.CreateRequest{}, err
	}
	if len(args) == 0 {
		if flags.rootDisk == "" {
			return vm.CreateRequest{}, fmt.Errorf("either IMAGE or --root-disk is required")
		}
		dataDisks, err := parseDataDisks(flags.dataDisks)
		if err != nil {
			return vm.CreateRequest{}, err
		}
		return vm.CreateRequest{Name: flags.name, RootDisk: flags.rootDisk, Kernel: flags.kernel, Initrd: flags.initrd, Firmware: flags.firmware, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedNetworkFlags(flags.networks), DataDisks: dataDisks, SharedMemory: flags.sharedMemory, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}, nil
	}
	if flags.rootDisk != "" || flags.kernel != "" || flags.initrd != "" || flags.firmware != "" {
		return vm.CreateRequest{}, fmt.Errorf("IMAGE cannot be combined with --root-disk, --kernel, --initrd, or --firmware")
	}
	stores, err := configuredStores(cfg)
	if err != nil {
		return vm.CreateRequest{}, err
	}
	if stores.Metadata != nil {
		defer func() { _ = stores.Metadata.Close() }()
	}
	image, err := stores.Images.Inspect(args[0])
	if err != nil {
		return vm.CreateRequest{}, fmt.Errorf("resolve image %q: %w", args[0], err)
	}
	if image.OCI == nil && image.RootDisk.Path == "" {
		return vm.CreateRequest{}, fmt.Errorf("image %q has no root disk", args[0])
	}
	if image.OCI != nil {
		return newOCIImageCreateRequest(flags, image, cfg)
	}
	if image.RootDisk.Format != vm.FormatQCOW2 {
		return vm.CreateRequest{}, fmt.Errorf("image %q root disk format %q cannot use a qcow2 overlay", image.Name, image.RootDisk.Format)
	}
	if image.RootDisk.SHA256 == "" {
		return vm.CreateRequest{}, fmt.Errorf("image %q root disk has no pinned sha256 digest", image.Name)
	}
	digest := image.RootDisk.SHA256
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	dataDisks, err := parseDataDisks(flags.dataDisks)
	if err != nil {
		return vm.CreateRequest{}, err
	}
	req := vm.CreateRequest{Name: flags.name, RootDisk: image.RootDisk.Path, Kernel: image.Boot.Kernel, Initrd: image.Boot.Initrd, Firmware: image.Boot.Firmware, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedNetworkFlags(flags.networks), DataDisks: dataDisks, SharedMemory: flags.sharedMemory, Image: &vm.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: digest}, StorageConfigs: []vm.StorageConfig{{ID: "root", Role: vm.StorageRoleCOW, Format: vm.FormatQCOW2, VirtualSizeBytes: image.RootDisk.VirtualSizeBytes, Base: &vm.StorageBase{Family: "cloudimg", ImageID: image.ID, Digest: digest, Format: image.RootDisk.Format, Path: image.RootDisk.Path}}}, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}
	if req.Firmware == "" && (req.Kernel == "" || req.Initrd == "") {
		return vm.CreateRequest{}, fmt.Errorf("image %q has no usable boot configuration", args[0])
	}
	return req, nil
}

func newOCIImageCreateRequest(flags createVMFlags, image *imagestore.ImageRecord, cfg config.Config) (vm.CreateRequest, error) {
	if image.Boot.Mode != "direct" || image.Boot.Kernel == "" || image.Boot.Initrd == "" {
		return vm.CreateRequest{}, fmt.Errorf("image %q has no OCI direct boot profile", image.Name)
	}
	cowSize, err := parseByteSize(defaultString(flags.storage, defaultOCIStorageSize))
	if err != nil {
		return vm.CreateRequest{}, err
	}
	memoryBytes, err := parseMemorySize(defaultString(flags.memory, defaultOCIMemorySize))
	if err != nil {
		return vm.CreateRequest{}, err
	}
	manifestDigest := image.OCI.DigestRef
	if _, digest, found := strings.Cut(manifestDigest, "@"); found {
		manifestDigest = digest
	}
	if manifestDigest == "" {
		return vm.CreateRequest{}, fmt.Errorf("image %q has no OCI manifest digest", image.Name)
	}
	storageConfigs := make([]vm.StorageConfig, 0, len(image.OCI.Layers)+1)
	layerDigests := make([]string, 0, len(image.OCI.Layers))
	for i, layer := range image.OCI.Layers {
		if layer.EROFS == nil || layer.EROFS.Path == "" {
			return vm.CreateRequest{}, fmt.Errorf("image %q layer %d has no EROFS blob", image.Name, i)
		}
		serial := layer.Serial
		if serial == "" {
			serial = vm.LayerSerial(i)
		}
		storageConfigs = append(storageConfigs, vm.StorageConfig{ID: vm.LayerID(i), Role: vm.StorageRoleLayer, Path: layer.EROFS.Path, Readonly: true, Format: vm.FormatRaw, Serial: serial, Filesystem: vm.FilesystemEROFS, SourceLayer: layer.Digest, VirtualSizeBytes: layer.EROFS.SizeBytes})
		layerDigests = append(layerDigests, layer.Digest)
	}
	storageConfigs = append(storageConfigs, vm.StorageConfig{ID: vm.StorageIDCOW, Role: vm.StorageRoleCOW, Format: vm.FormatRaw, Serial: vm.StorageSerialCOW, Filesystem: vm.FilesystemEXT4, VirtualSizeBytes: cowSize, Base: &vm.StorageBase{Family: vm.BaseFamilyOCI, ImageID: image.ID, Digest: manifestDigest, LayerDigests: append([]string(nil), layerDigests...)}})
	dataDisks, err := parseDataDisks(flags.dataDisks)
	if err != nil {
		return vm.CreateRequest{}, err
	}
	return vm.CreateRequest{Name: flags.name, Kernel: image.Boot.Kernel, Initrd: image.Boot.Initrd, KernelCmdline: image.Boot.Cmdline, CPUs: flags.cpus, MemoryBytes: memoryBytes, Networks: normalizedOCIImageNetworkFlags(flags.networks, cfg), DataDisks: dataDisks, SharedMemory: flags.sharedMemory, StorageConfigs: storageConfigs, Image: &vm.ImageRef{ID: image.ID, Name: image.Name, RootDisk: image.RootDisk.Path, BootMode: image.Boot.Mode, Digest: manifestDigest, LayerDigests: append([]string(nil), layerDigests...)}, RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir}, nil
}

func parseDataDisks(values []string) ([]vm.DataDiskRequest, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make([]vm.DataDiskRequest, 0, len(values))
	usedNames := make(map[string]struct{}, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.TrimSpace(key) == "name" && strings.TrimSpace(val) != "" {
				usedNames[strings.TrimSpace(val)] = struct{}{}
			}
		}
	}
	for _, value := range values {
		var disk vm.DataDiskRequest
		seenKeys := make(map[string]struct{})
		for _, part := range strings.Split(value, ",") {
			key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || (strings.TrimSpace(val) == "" && strings.TrimSpace(key) != "mount") {
				return nil, fmt.Errorf("--data-disk expects key=value fields: %q", value)
			}
			key = strings.TrimSpace(key)
			if _, exists := seenKeys[key]; exists {
				return nil, fmt.Errorf("--data-disk field %q repeated", key)
			}
			seenKeys[key] = struct{}{}
			switch key {
			case "name":
				disk.Name = strings.TrimSpace(val)
			case "size":
				size, err := parsePositiveByteSize("--data-disk size", val)
				if err != nil {
					return nil, err
				}
				if size < 16<<20 {
					return nil, fmt.Errorf("--data-disk size %s is below the 16M minimum", val)
				}
				disk.SizeBytes = size
			case "fstype":
				disk.Filesystem = strings.TrimSpace(val)
				if disk.Filesystem != vm.FilesystemEXT4 && disk.Filesystem != vm.FilesystemNone {
					return nil, fmt.Errorf("--data-disk: unsupported fstype %q", disk.Filesystem)
				}
			case "mount":
				disk.MountPoint = strings.TrimSpace(val)
				disk.MountSet = true
			case "directio":
				parsed, err := parseOptionalBool(val)
				if err != nil {
					return nil, fmt.Errorf("--data-disk directio: %w", err)
				}
				disk.DirectIO = parsed
			default:
				return nil, fmt.Errorf("--data-disk has unknown field %q", key)
			}
		}
		explicitName := disk.Name != ""
		if disk.Name == "" {
			for index := 1; ; index++ {
				candidate := fmt.Sprintf("data%d", index)
				if _, exists := usedNames[candidate]; !exists {
					disk.Name = candidate
					usedNames[candidate] = struct{}{}
					break
				}
			}
		}
		if explicitName && countDataDiskName(values, disk.Name) > 1 {
			return nil, fmt.Errorf("--data-disk name %q duplicated", disk.Name)
		}
		result = append(result, disk)
	}
	return result, nil
}

func countDataDiskName(values []string, name string) int {
	count := 0
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.TrimSpace(key) == "name" && strings.TrimSpace(val) == name {
				count++
			}
		}
	}
	return count
}

func parseOptionalBool(value string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "yes":
		parsed := true
		return &parsed, nil
	case "off", "false", "no":
		parsed := false
		return &parsed, nil
	case "auto":
		return nil, nil
	default:
		return nil, fmt.Errorf("expected on, off, or auto")
	}
}
