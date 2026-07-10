// Package cloudhypervisor implements KumaBox's Cloud Hypervisor backend.
//
// The backend renders an auditable JSON config beside the VM runtime files and
// then starts the cloud-hypervisor process with the corresponding CLI arguments.
package cloudhypervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/metadata"
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
	Disks        []Disk      `json:"disks"`
	Nets         []Net       `json:"nets,omitempty"`
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

// Disk is one block device passed to Cloud Hypervisor.
type Disk struct {
	Path      string `json:"path"`
	Readonly  bool   `json:"readonly"`
	ImageType string `json:"imageType,omitempty"`
	Serial    string `json:"serial,omitempty"`
}

// Net is one virtio-net device backed by a host TAP interface.
type Net struct {
	TAP       string `json:"tap"`
	MAC       string `json:"mac"`
	NumQueues int    `json:"numQueues"`
	QueueSize int    `json:"queueSize"`
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
	serialLog := filepath.Join(rec.LogDir, "console.log")
	cpus := vmCPUs(rec)

	args := []string{
		"--api-socket", apiSocket,
		"--cpus", fmt.Sprintf("boot=%d", cpus),
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
	for _, disk := range launchDisks(rec) {
		args = append(args, "--disk", diskArg(disk))
	}
	args = append(args, "--serial", "file="+serialLog, "--console", "off")
	if meta := activeMetadata(rec); meta != nil && meta.CidataDisk != "" {
		args = append(args, "--disk", "path="+meta.CidataDisk+",readonly=on,image_type=raw")
	}
	for _, net := range newNets(rec) {
		netArg := fmt.Sprintf("tap=%s,mac=%s", net.TAP, net.MAC)
		if net.NumQueues > 0 {
			netArg += fmt.Sprintf(",num_queues=%d", net.NumQueues)
		}
		if net.QueueSize > 0 {
			netArg += fmt.Sprintf(",queue_size=%d", net.QueueSize)
		}
		args = append(args, "--net", netArg)
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
		Disks:        newDisks(rec),
		Nets:         newNets(rec),
		Serial:       Serial{Path: serialLog},
		Console:      Console{Mode: "off"},
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

func vmCPUs(rec *vmstore.VMRecord) int {
	if rec == nil || rec.CPUs <= 0 {
		return 1
	}
	return rec.CPUs
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
			TAP:       nc.TAP,
			MAC:       nc.MAC,
			NumQueues: nc.NumQueues,
			QueueSize: nc.QueueSize,
		})
	}
	return nets
}

func newDisks(rec *vmstore.VMRecord) []Disk {
	disks := launchDisks(rec)
	if meta := activeMetadata(rec); meta != nil && meta.CidataDisk != "" {
		disks = append(disks, Disk{
			Path:      meta.CidataDisk,
			Readonly:  true,
			ImageType: "raw",
		})
	}
	return disks
}

func launchDisks(rec *vmstore.VMRecord) []Disk {
	if len(rec.StorageConfigs) > 0 {
		disks := make([]Disk, 0, len(rec.StorageConfigs))
		for _, cfg := range rec.StorageConfigs {
			disks = append(disks, Disk{
				Path:      cfg.Path,
				Readonly:  cfg.Readonly,
				ImageType: cfg.ImageType,
				Serial:    cfg.Serial,
			})
		}
		return disks
	}
	return []Disk{newRootDisk(rec)}
}

func diskArg(disk Disk) string {
	arg := "path=" + disk.Path
	if disk.Readonly {
		arg += ",readonly=on"
	}
	if disk.ImageType != "" {
		arg += ",image_type=" + disk.ImageType
	}
	if disk.Serial != "" {
		arg += ",serial=" + disk.Serial
	}
	return arg
}

func kernelCmdline(rec *vmstore.VMRecord) string {
	cmdline := rec.KernelCmdline
	if cmdline == "" {
		cmdline = defaultKernelCmdline
	}
	layers := make([]string, 0)
	cow := ""
	for _, cfg := range rec.StorageConfigs {
		switch cfg.Type {
		case "layer":
			if cfg.Serial != "" {
				layers = append(layers, cfg.Serial)
			}
		case "cow":
			cow = cfg.Serial
		}
	}
	cmdline = strings.ReplaceAll(cmdline, "{{layers}}", strings.Join(layers, ","))
	cmdline = strings.ReplaceAll(cmdline, "{{cow}}", cow)
	return cmdline
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
	}
	return disk
}

func rootDiskImageType(rec *vmstore.VMRecord) string {
	if rec.Firmware != "" {
		return "qcow2"
	}
	if filepath.Ext(rec.RootDisk) == ".qcow2" {
		return "qcow2"
	}
	return ""
}
