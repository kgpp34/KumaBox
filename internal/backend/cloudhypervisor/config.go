package cloudhypervisor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/metadata"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const defaultKernelCmdline = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw"

type Config struct {
	Binary       string      `json:"binary"`
	APISocket    string      `json:"apiSocket"`
	APITimeoutMs int         `json:"apiTimeoutMs"`
	PIDFile      string      `json:"pidFile"`
	StdoutLog    string      `json:"stdoutLog"`
	StderrLog    string      `json:"stderrLog"`
	Kernel       *Kernel     `json:"kernel,omitempty"`
	Initramfs    *Initramfs  `json:"initramfs,omitempty"`
	Firmware     *Firmware   `json:"firmware,omitempty"`
	Disks        []Disk      `json:"disks"`
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

type Disk struct {
	Path      string `json:"path"`
	Readonly  bool   `json:"readonly"`
	ImageType string `json:"imageType,omitempty"`
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

type Renderer struct {
	cfg config.Config
}

func NewRenderer(cfg config.Config) Renderer {
	return Renderer{cfg: cfg}
}

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
	if rec.Metadata != nil && rec.Metadata.Type == "nocloud" {
		if err := metadata.WriteNoCloud(rec.Metadata.CidataDir, rec.Metadata.CidataDisk, metadata.Config{
			InstanceID: rec.ID,
			Hostname:   rec.Name,
			Username:   "kumabox",
		}); err != nil {
			return fmt.Errorf("render NoCloud metadata: %w", err)
		}
	}

	rendered := NewConfig(r.cfg, rec)
	if err := fileutil.WriteJSONAtomic(rec.Config, rendered, ".cloud-hypervisor-*.tmp"); err != nil {
		return fmt.Errorf("write Cloud Hypervisor config: %w", err)
	}
	return nil
}

func NewConfig(cfg config.Config, rec *vmstore.VMRecord) Config {
	apiSocket := filepath.Join(rec.RunDir, "ch.sock")
	stdoutLog := filepath.Join(rec.LogDir, "cloud-hypervisor.stdout.log")
	stderrLog := filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log")
	serialLog := filepath.Join(rec.LogDir, "console.log")

	args := []string{
		"--api-socket", apiSocket,
	}
	if rec.Firmware != "" {
		args = append(args, "--firmware", rec.Firmware)
	} else {
		args = append(args,
			"--kernel", rec.Kernel,
			"--initramfs", rec.Initrd,
			"--cmdline", defaultKernelCmdline,
		)
	}
	diskArg := "path=" + rec.RootDisk
	if imageType := rootDiskImageType(rec); imageType != "" {
		diskArg += ",image_type=" + imageType
	}
	args = append(args,
		"--disk", diskArg,
		"--serial", "file="+serialLog,
		"--console", "off",
	)
	if rec.Metadata != nil && rec.Metadata.CidataDisk != "" {
		args = append(args, "--disk", "path="+rec.Metadata.CidataDisk+",readonly=on,image_type=raw")
	}

	rendered := Config{
		Binary:       cfg.Backend.CloudHypervisor.Binary,
		APISocket:    apiSocket,
		APITimeoutMs: cfg.Backend.CloudHypervisor.APISocketTimeoutMS,
		PIDFile:      filepath.Join(rec.RunDir, "ch.pid"),
		StdoutLog:    stdoutLog,
		StderrLog:    stderrLog,
		Disks:        newDisks(rec),
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
			Cmdline: defaultKernelCmdline,
		}
		rendered.Initramfs = &Initramfs{Path: rec.Initrd}
	}
	return rendered
}

func newDisks(rec *vmstore.VMRecord) []Disk {
	disks := []Disk{newRootDisk(rec)}
	if rec.Metadata != nil && rec.Metadata.CidataDisk != "" {
		disks = append(disks, Disk{
			Path:      rec.Metadata.CidataDisk,
			Readonly:  true,
			ImageType: "raw",
		})
	}
	return disks
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
