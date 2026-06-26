package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const defaultKernelCmdline = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw"

type Config struct {
	Binary      string      `json:"binary"`
	APISocket   string      `json:"apiSocket"`
	PIDFile     string      `json:"pidFile"`
	StdoutLog   string      `json:"stdoutLog"`
	StderrLog   string      `json:"stderrLog"`
	Kernel      Kernel      `json:"kernel"`
	Initramfs   Initramfs   `json:"initramfs"`
	Disks       []Disk      `json:"disks"`
	Serial      Serial      `json:"serial"`
	Console     Console     `json:"console"`
	Args        []string    `json:"args"`
	Annotations Annotations `json:"annotations"`
}

type Kernel struct {
	Path    string `json:"path"`
	Cmdline string `json:"cmdline"`
}

type Initramfs struct {
	Path string `json:"path"`
}

type Disk struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
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

func RenderConfig(cfg config.Config, rec *vmstore.VMRecord) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	if err := os.MkdirAll(rec.RunDir, 0o755); err != nil {
		return fmt.Errorf("create VM run dir: %w", err)
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		return fmt.Errorf("create VM log dir: %w", err)
	}

	rendered := NewConfig(cfg, rec)
	raw, err := json.MarshalIndent(rendered, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal Cloud Hypervisor config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(rec.Config), ".cloud-hypervisor-*.tmp")
	if err != nil {
		return fmt.Errorf("create Cloud Hypervisor config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write Cloud Hypervisor config temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync Cloud Hypervisor config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Cloud Hypervisor config temp file: %w", err)
	}
	if err := os.Rename(tmpPath, rec.Config); err != nil {
		return fmt.Errorf("commit Cloud Hypervisor config: %w", err)
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
		"--kernel", rec.Kernel,
		"--initramfs", rec.Initrd,
		"--cmdline", defaultKernelCmdline,
		"--disk", "path=" + rec.RootDisk,
		"--serial", "file=" + serialLog,
		"--console", "off",
	}

	return Config{
		Binary:    cfg.Backend.CloudHypervisor.Binary,
		APISocket: apiSocket,
		PIDFile:   filepath.Join(rec.RunDir, "ch.pid"),
		StdoutLog: stdoutLog,
		StderrLog: stderrLog,
		Kernel: Kernel{
			Path:    rec.Kernel,
			Cmdline: defaultKernelCmdline,
		},
		Initramfs: Initramfs{Path: rec.Initrd},
		Disks: []Disk{
			{Path: rec.RootDisk, Readonly: false},
		},
		Serial:  Serial{Path: serialLog},
		Console: Console{Mode: "off"},
		Args:    args,
		Annotations: Annotations{
			VMID:   rec.ID,
			VMName: rec.Name,
		},
	}
}
