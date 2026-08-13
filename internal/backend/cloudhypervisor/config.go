// Package cloudhypervisor implements KumaBox's Cloud Hypervisor backend.
//
// The backend renders an auditable JSON config beside the VM runtime files and
// then starts the cloud-hypervisor process with the corresponding CLI arguments.
package cloudhypervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/metadata"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const defaultKernelCmdline = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw"

// Config is the rendered Cloud Hypervisor launch plan.
//
// It is written to the VM run directory before start so users and verification
// scripts can inspect exactly which disks, networks, sockets, and logs were
// handed to the VMM.
type Config struct {
	Binary       string      `json:"binary"`
	APISocket    string      `json:"apiSocket"`
	APITimeoutMs int         `json:"apiTimeoutMs"`
	PIDFile      string      `json:"pidFile"`
	StdoutLog    string      `json:"stdoutLog"`
	StderrLog    string      `json:"stderrLog"`
	NetnsPath    string      `json:"netnsPath,omitempty"`
	Kernel       *Kernel     `json:"kernel,omitempty"`
	Initramfs    *Initramfs  `json:"initramfs,omitempty"`
	Firmware     *Firmware   `json:"firmware,omitempty"`
	CPUs         CPUs        `json:"cpus"`
	Memory       Memory      `json:"memory"`
	Disks        []Disk      `json:"disks"`
	Nets         []Net       `json:"nets,omitempty"`
	Vsock        *Vsock      `json:"vsock,omitempty"`
	Serial       Serial      `json:"serial"`
	Console      Console     `json:"console"`
	Args         []string    `json:"args"`
	Annotations  Annotations `json:"annotations"`
}

type Kernel struct {
	Path    string `json:"path"`
	Cmdline string `json:"cmdline"`
}

type Initramfs struct {
	Path string `json:"path"`
}

type Firmware struct {
	Path string `json:"path"`
}

type CPUs struct {
	Boot int `json:"boot"`
}

type Memory struct {
	Size   int64 `json:"size"`
	Shared bool  `json:"shared,omitempty"`
}

// Disk is one block device passed to Cloud Hypervisor.
type Disk struct {
	Path          string          `json:"path"`
	Readonly      bool            `json:"readonly"`
	DirectIO      bool            `json:"direct,omitempty"`
	Sparse        bool            `json:"sparse,omitempty"`
	ImageType     string          `json:"imageType,omitempty"`
	BackingFiles  bool            `json:"backingFiles,omitempty"`
	NumQueues     int             `json:"numQueues,omitempty"`
	QueueSize     int             `json:"queueSize,omitempty"`
	QueueAffinity []QueueAffinity `json:"queueAffinity,omitempty"`
	Serial        string          `json:"serial,omitempty"`
}

type QueueAffinity struct {
	QueueIndex int   `json:"queueIndex"`
	HostCPUs   []int `json:"hostCPUs"`
}

// Net is one virtio-net device backed by a host TAP interface.
type Net struct {
	TAP         string `json:"tap"`
	MAC         string `json:"mac"`
	NumQueues   int    `json:"numQueues"`
	QueueSize   int    `json:"queueSize"`
	OffloadTSO  bool   `json:"offloadTSO"`
	OffloadUFO  bool   `json:"offloadUFO"`
	OffloadCsum bool   `json:"offloadCsum"`
}

type Vsock struct {
	CID    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

type Serial struct {
	Path string `json:"path"`
}

type Console struct {
	Mode string `json:"mode"`
}

type Annotations struct {
	VMID   string `json:"vmId"`
	VMName string `json:"vmName"`
}

// Renderer writes Cloud Hypervisor config and first-boot metadata.
type Renderer struct {
	cfg config.Config
}

// NewRenderer returns a renderer using the supplied process configuration.
func NewRenderer(cfg config.Config) Renderer {
	return Renderer{cfg: cfg}
}

// RenderConfig writes all files required before starting Cloud Hypervisor.
//
// For cloud-image boots it also regenerates the NoCloud CIDATA disk from the
// VM's current network configs, so the guest sees the same IP/MAC assignment
// that Cloud Hypervisor receives.
func (r Renderer) RenderConfig(rec *vmstore.VMRecord) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	if err := os.MkdirAll(rec.RunDir, 0o755); err != nil {
		return fmt.Errorf("create VM run dir: %w", err)
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		return fmt.Errorf("create VM log dir: %w", err)
	}
	if meta := activeMetadata(rec); meta != nil && meta.Type == "nocloud" {
		if err := metadata.WriteNoCloud(meta.CidataDir, meta.CidataDisk, metadata.Config{
			InstanceID: rec.ID,
			Hostname:   rec.Name,
			Username:   "kumabox",
			Networks:   metadataNetworks(rec),
			Mounts:     metadataMounts(rec),
		}); err != nil {
			return fmt.Errorf("render NoCloud metadata: %w", err)
		}
	}
	if err := validateNetworkQueues(rec); err != nil {
		return err
	}

	rendered := NewConfig(r.cfg, rec)
	if err := fileutil.WriteJSONAtomic(rec.Config, rendered, ".cloud-hypervisor-*.tmp"); err != nil {
		return fmt.Errorf("write Cloud Hypervisor config: %w", err)
	}
	return nil
}

func metadataMounts(rec *vmstore.VMRecord) []metadata.Mount {
	if rec == nil {
		return nil
	}
	mounts := make([]metadata.Mount, 0)
	for _, storage := range rec.StorageConfigs {
		if storage.EffectiveRole() != vmstore.StorageRoleData || storage.MountPoint == "" || storage.Filesystem == "" || storage.Filesystem == vmstore.FilesystemNone {
			continue
		}
		mounts = append(mounts, metadata.Mount{
			Device:     "/dev/disk/by-id/virtio-" + storage.Serial,
			MountPoint: storage.MountPoint,
			Filesystem: storage.Filesystem,
			Options:    "defaults,nofail",
		})
	}
	return mounts
}

func validateNetworkQueues(rec *vmstore.VMRecord) error {
	for _, nc := range rec.NetworkConfigs {
		if nc.NumQueues > 0 && nc.NumQueues < 2 {
			return fmt.Errorf("network %s numQueues must be at least 2", nc.ID)
		}
	}
	return nil
}

func metadataNetworks(rec *vmstore.VMRecord) []metadata.Network {
	networks := make([]metadata.Network, 0, len(rec.NetworkConfigs))
	for _, nc := range rec.NetworkConfigs {
		if nc.MAC == "" || nc.Network == nil || nc.Network.IP == "" {
			continue
		}
		networks = append(networks, metadata.Network{
			MAC:     nc.MAC,
			IP:      nc.Network.IP,
			Prefix:  nc.Network.Prefix,
			Gateway: nc.Network.Gateway,
			DNS:     append([]string(nil), nc.Network.DNS...),
		})
	}
	return networks
}

// NewConfig derives Cloud Hypervisor arguments from a VM record.
//
// The function is pure with respect to the filesystem; Renderer.RenderConfig is
// responsible for writing the returned config and any metadata sidecars.
func NewConfig(cfg config.Config, rec *vmstore.VMRecord) Config {
	apiSocket := filepath.Join(rec.RunDir, "ch.sock")
	stdoutLog := filepath.Join(rec.LogDir, "cloud-hypervisor.stdout.log")
	stderrLog := filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log")
	cpus := vmCPUs(rec)

	args := []string{
		"--api-socket", apiSocket,
		"--cpus", fmt.Sprintf("boot=%d", cpus),
		"--memory", memoryArg(rec),
	}
	cmdline := kernelCmdline(rec)
	if rec.Firmware != "" {
		args = append(args, "--firmware", rec.Firmware)
	} else {
		args = append(args,
			"--kernel", rec.Kernel,
			"--initramfs", rec.Initrd,
			"--cmdline", cmdline,
		)
	}
	disks := newDisks(cfg, rec)
	if len(disks) > 0 {
		args = append(args, "--disk")
		for _, disk := range disks {
			args = append(args, diskArg(disk))
		}
	}
	if rec.Firmware == "" {
		args = append(args, "--serial", "off", "--console", "pty")
	} else {
		args = append(args, "--serial", "file="+filepath.Join(rec.LogDir, "console.log"), "--console", "off")
	}
	nets := newNets(rec)
	if len(nets) > 0 {
		args = append(args, "--net")
	}
	for _, net := range nets {
		netArg := fmt.Sprintf("tap=%s,mac=%s", net.TAP, net.MAC)
		if net.NumQueues > 0 {
			netArg += fmt.Sprintf(",num_queues=%d", net.NumQueues)
		}
		if net.QueueSize > 0 {
			netArg += fmt.Sprintf(",queue_size=%d", net.QueueSize)
		}
		if net.OffloadTSO {
			netArg += ",offload_tso=on"
		}
		if net.OffloadUFO {
			netArg += ",offload_ufo=on"
		}
		if net.OffloadCsum {
			netArg += ",offload_csum=on"
		}
		args = append(args, netArg)
	}
	vsock := newVsock(rec)
	if vsock != nil {
		args = append(args, "--vsock", fmt.Sprintf("cid=%d,socket=%s", vsock.CID, vsock.Socket))
	}

	rendered := Config{
		Binary:       cfg.Backend.CloudHypervisor.Binary,
		APISocket:    apiSocket,
		APITimeoutMs: cfg.Backend.CloudHypervisor.APISocketTimeoutMS,
		PIDFile:      filepath.Join(rec.RunDir, "ch.pid"),
		StdoutLog:    stdoutLog,
		StderrLog:    stderrLog,
		NetnsPath:    netnsPath(rec),
		CPUs:         CPUs{Boot: cpus},
		Memory:       Memory{Size: vmMemoryBytes(rec), Shared: rec.SharedMemory},
		Disks:        disks,
		Nets:         nets,
		Vsock:        vsock,
		Console:      Console{Mode: consoleMode(rec)},
		Args:         args,
		Annotations: Annotations{
			VMID:   rec.ID,
			VMName: rec.Name,
		},
	}
	if rec.Firmware != "" {
		rendered.Firmware = &Firmware{Path: rec.Firmware}
	} else {
		rendered.Kernel = &Kernel{
			Path:    rec.Kernel,
			Cmdline: cmdline,
		}
		rendered.Initramfs = &Initramfs{Path: rec.Initrd}
	}
	return rendered
}

// ValidateConfig checks the pure launch plan without touching host resources.
func ValidateConfig(launch Config) error {
	if launch.Binary == "" {
		return errors.New("cloud-hypervisor binary is empty")
	}
	if launch.APISocket == "" || launch.PIDFile == "" {
		return errors.New("cloud-hypervisor runtime paths are incomplete")
	}
	if launch.CPUs.Boot <= 0 || launch.Memory.Size <= 0 {
		return errors.New("cloud-hypervisor CPU and memory must be positive")
	}
	if launch.Firmware != nil && launch.Firmware.Path == "" {
		return errors.New("cloud-hypervisor firmware path is empty")
	}
	if launch.Firmware == nil && (launch.Kernel == nil || launch.Initramfs == nil || launch.Kernel.Path == "" || launch.Initramfs.Path == "") {
		return errors.New("cloud-hypervisor boot configuration is incomplete")
	}
	for _, disk := range launch.Disks {
		if disk.Path == "" {
			return errors.New("cloud-hypervisor disk path is empty")
		}
	}
	for _, network := range launch.Nets {
		if network.TAP == "" || network.MAC == "" {
			return errors.New("cloud-hypervisor network configuration is incomplete")
		}
	}
	return nil
}

func newVsock(rec *vmstore.VMRecord) *Vsock {
	if rec == nil || rec.VsockSocket == "" {
		return nil
	}
	return &Vsock{
		CID:    3,
		Socket: rec.VsockSocket,
	}
}

func vmCPUs(rec *vmstore.VMRecord) int {
	if rec == nil || rec.CPUs <= 0 {
		return 1
	}
	return rec.CPUs
}

func vmMemoryBytes(rec *vmstore.VMRecord) int64 {
	return rec.EffectiveMemoryBytes()
}

func memoryArg(rec *vmstore.VMRecord) string {
	value := fmt.Sprintf("size=%d", vmMemoryBytes(rec))
	if rec.SharedMemory {
		value += ",shared=on"
	}
	return value
}

func netnsPath(rec *vmstore.VMRecord) string {
	for _, nc := range rec.NetworkConfigs {
		if nc.NetnsPath != "" {
			return nc.NetnsPath
		}
	}
	return ""
}

func newNets(rec *vmstore.VMRecord) []Net {
	nets := make([]Net, 0, len(rec.NetworkConfigs))
	for _, nc := range rec.NetworkConfigs {
		if nc.TAP == "" {
			continue
		}
		nets = append(nets, Net{
			TAP:         nc.TAP,
			MAC:         nc.MAC,
			NumQueues:   nc.NumQueues,
			QueueSize:   nc.QueueSize,
			OffloadTSO:  true,
			OffloadUFO:  true,
			OffloadCsum: true,
		})
	}
	return nets
}

func newDisks(cfg config.Config, rec *vmstore.VMRecord) []Disk {
	disks := launchDisks(cfg, rec)
	if meta := activeMetadata(rec); meta != nil && meta.CidataDisk != "" {
		disks = append(disks, configureDisk(cfg, rec, Disk{
			Path:      meta.CidataDisk,
			Readonly:  true,
			ImageType: vmstore.FormatRaw,
		}, nil))
	}
	return disks
}

func launchDisks(cfg config.Config, rec *vmstore.VMRecord) []Disk {
	if len(rec.StorageConfigs) > 0 {
		disks := make([]Disk, 0, len(rec.StorageConfigs))
		for _, storageCfg := range rec.StorageConfigs {
			imageType := storageCfg.EffectiveFormat()
			disks = append(disks, configureDisk(cfg, rec, Disk{
				Path:         storageCfg.Path,
				Readonly:     storageCfg.Readonly,
				ImageType:    imageType,
				BackingFiles: imageType == vmstore.FormatQCOW2 && !storageCfg.Readonly,
				Serial:       storageCfg.Serial,
			}, &storageCfg))
		}
		return disks
	}
	return []Disk{configureDisk(cfg, rec, newRootDisk(rec), nil)}
}

func configureDisk(cfg config.Config, rec *vmstore.VMRecord, disk Disk, storage *vmstore.StorageConfig) Disk {
	disk.NumQueues = vmCPUs(rec)
	disk.QueueSize = cfg.Backend.CloudHypervisor.DiskQueueSize
	if disk.Readonly {
		return disk
	}
	disk.DirectIO = !cfg.Backend.CloudHypervisor.NoDirectIO
	if storage != nil && storage.DirectIO != nil {
		disk.DirectIO = *storage.DirectIO
	}
	disk.Sparse = disk.ImageType != vmstore.FormatQCOW2
	if disk.NumQueues > 1 {
		disk.QueueAffinity = make([]QueueAffinity, disk.NumQueues)
		for queue := range disk.QueueAffinity {
			disk.QueueAffinity[queue] = QueueAffinity{QueueIndex: queue, HostCPUs: []int{queue}}
		}
	}
	return disk
}

func diskArg(disk Disk) string {
	arg := "path=" + disk.Path
	if disk.Readonly {
		arg += ",readonly=on"
	}
	if disk.DirectIO {
		arg += ",direct=on"
	}
	if disk.Sparse {
		arg += ",sparse=on"
	}
	if disk.ImageType != "" {
		arg += ",image_type=" + disk.ImageType
	}
	if disk.BackingFiles {
		arg += ",backing_files=on"
	}
	if disk.NumQueues > 0 {
		arg += fmt.Sprintf(",num_queues=%d", disk.NumQueues)
	}
	if disk.QueueSize > 0 {
		arg += fmt.Sprintf(",queue_size=%d", disk.QueueSize)
	}
	if len(disk.QueueAffinity) > 0 {
		arg += ",queue_affinity=" + queueAffinityArg(disk.QueueAffinity)
	}
	if disk.Serial != "" {
		arg += ",serial=" + disk.Serial
	}
	return arg
}

func queueAffinityArg(affinities []QueueAffinity) string {
	parts := make([]string, len(affinities))
	for i, affinity := range affinities {
		cpus := make([]string, len(affinity.HostCPUs))
		for j, cpu := range affinity.HostCPUs {
			cpus[j] = fmt.Sprintf("%d", cpu)
		}
		parts[i] = fmt.Sprintf("%d@[%s]", affinity.QueueIndex, strings.Join(cpus, ","))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func kernelCmdline(rec *vmstore.VMRecord) string {
	cmdline := rec.KernelCmdline
	if cmdline == "" {
		cmdline = defaultKernelCmdline
	}
	if rec.Firmware == "" {
		cmdline = directBootConsoleCmdline(cmdline)
	}
	layers := make([]string, 0)
	cow := ""
	for _, cfg := range rec.StorageConfigs {
		switch cfg.EffectiveRole() {
		case vmstore.StorageRoleLayer:
			if cfg.Serial != "" {
				layers = append(layers, cfg.Serial)
			}
		case vmstore.StorageRoleCOW:
			cow = cfg.Serial
		}
	}
	for left, right := 0, len(layers)-1; left < right; left, right = left+1, right-1 {
		layers[left], layers[right] = layers[right], layers[left]
	}
	cmdline = strings.ReplaceAll(cmdline, "{{layers}}", strings.Join(layers, ","))
	cmdline = strings.ReplaceAll(cmdline, "{{cow}}", cow)
	if len(rec.StorageConfigs) > 0 {
		cmdline += directBootNetworkCmdline(rec)
	}
	return cmdline
}

func directBootConsoleCmdline(cmdline string) string {
	fields := strings.Fields(cmdline)
	for index, field := range fields {
		if strings.HasPrefix(field, "console=") {
			fields[index] = "console=hvc0"
			return strings.Join(fields, " ")
		}
	}
	return strings.Join(append([]string{"console=hvc0"}, fields...), " ")
}

func consoleMode(rec *vmstore.VMRecord) string {
	if rec != nil && rec.Firmware == "" {
		return "pty"
	}
	return "off"
}

func directBootNetworkCmdline(rec *vmstore.VMRecord) string {
	var b strings.Builder
	if rec.Name != "" {
		b.WriteString(" kumabox.hostname=")
		b.WriteString(rec.Name)
	}
	if len(rec.NetworkConfigs) == 0 {
		return b.String()
	}
	b.WriteString(" net.ifnames=0")
	for i, nc := range rec.NetworkConfigs {
		if nc.Network == nil || nc.Network.IP == "" {
			continue
		}
		b.WriteString(" ip=")
		b.WriteString(nc.Network.IP)
		b.WriteString("::")
		b.WriteString(nc.Network.Gateway)
		b.WriteString(":")
		b.WriteString(prefixNetmask(nc.Network.Prefix))
		b.WriteString(":")
		b.WriteString(rec.Name)
		b.WriteString(":")
		b.WriteString(guestNICName(nc.IfName, i))
		b.WriteString(":off")
		for _, dns := range firstDNS(nc.Network.DNS, 2) {
			b.WriteString(":")
			b.WriteString(dns)
		}
	}
	return b.String()
}

func guestNICName(ifName string, index int) string {
	if ifName != "" {
		return ifName
	}
	return kbnetwork.GuestInterfaceName(index)
}

func prefixNetmask(prefix int) string {
	mask := net.CIDRMask(prefix, 32)
	if mask == nil {
		return "255.255.255.0"
	}
	return net.IP(mask).String()
}

func firstDNS(values []string, max int) []string {
	out := make([]string, 0, max)
	for _, value := range values {
		if value == "" {
			continue
		}
		out = append(out, value)
		if len(out) == max {
			break
		}
	}
	return out
}

func activeMetadata(rec *vmstore.VMRecord) *vmstore.Metadata {
	if rec == nil || rec.FirstBooted {
		return nil
	}
	return rec.Metadata
}

func newRootDisk(rec *vmstore.VMRecord) Disk {
	disk := Disk{Path: rec.RootDisk, Readonly: false}
	if imageType := rootDiskImageType(rec); imageType != "" {
		disk.ImageType = imageType
		disk.BackingFiles = imageType == vmstore.FormatQCOW2
	}
	return disk
}

func rootDiskImageType(rec *vmstore.VMRecord) string {
	if rec.Firmware != "" {
		return vmstore.FormatQCOW2
	}
	if filepath.Ext(rec.RootDisk) == ".qcow2" {
		return vmstore.FormatQCOW2
	}
	return ""
}
