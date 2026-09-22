// Package vmm defines launch plans and runtime process facts shared by the
// application service and virtual-machine-monitor adapters. It contains no
// lifecycle persistence or CLI presentation.
package vmm

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/types"
)

const (
	// LayerSerialPrefix identifies immutable EROFS disks by manifest position.
	LayerSerialPrefix = "kumabox-layer"
	// COWSerial identifies the sandbox-private ext4 overlay disk.
	COWSerial = "kumabox-cow"
	// VsockGuestCID is safe because every sandbox has a private host Unix socket.
	VsockGuestCID uint32 = 3
)

// Disk is one block device in VMM attachment order.
type Disk struct {
	// Path is an absolute managed artifact path on the host.
	Path string
	// Serial is the stable guest-visible identity used by early userspace.
	Serial string
	// ReadOnly protects shared image layers from guest writes.
	ReadOnly bool
}

// LaunchPlan is a complete, immutable request for one VMM process.
type LaunchPlan struct {
	// SandboxID owns every runtime path and process created from the plan.
	SandboxID types.SandboxID
	// Generation is the durable Starting generation that owns this launch.
	Generation uint64
	// CPUs is the number of boot vCPUs.
	CPUs uint32
	// Memory is guest RAM in bytes.
	Memory int64
	// BootProfile selects the host/guest direct-boot contract.
	BootProfile types.BootProfile
	// Kernel is the verified direct-boot kernel artifact.
	Kernel string
	// Initrd is the verified early-userspace artifact.
	Initrd string
	// Cmdline carries the versioned boot profile parameters.
	Cmdline string
	// Disks are attached base-to-top followed by the private COW disk.
	Disks []Disk
	// Network is the validated host-to-VMM handoff. Its zero value disables
	// network attachment and namespace entry.
	Network types.NetworkSetup
}

// Validate rejects incomplete plans before an adapter creates runtime state.
func (p LaunchPlan) Validate() error {
	if _, err := types.ParseSandboxID(p.SandboxID.String()); err != nil {
		return err
	}
	if p.Generation == 0 || p.CPUs == 0 || p.Memory <= 0 {
		return errors.New("launch generation, CPUs, and memory must be positive")
	}
	if p.BootProfile != types.BootProfileOverlayV1 {
		return fmt.Errorf("unsupported boot profile %q", p.BootProfile)
	}
	if !filepath.IsAbs(p.Kernel) || !filepath.IsAbs(p.Initrd) || p.Cmdline == "" || len(p.Disks) < 2 {
		return errors.New("launch plan requires absolute boot artifacts, a cmdline, image layers, and COW")
	}
	seen := make(map[string]bool, len(p.Disks))
	for position, disk := range p.Disks {
		if !filepath.IsAbs(disk.Path) || disk.Serial == "" || seen[disk.Serial] {
			return errors.New("launch plan contains an invalid or duplicate disk")
		}
		seen[disk.Serial] = true
		last := position == len(p.Disks)-1
		if last != (disk.Serial == COWSerial && !disk.ReadOnly) {
			return errors.New("launch plan must end with one writable kumabox-cow disk")
		}
		if !last && (!disk.ReadOnly || disk.Serial != fmt.Sprintf("%s%d", LayerSerialPrefix, position)) {
			return errors.New("image disks must be read-only and serialed by manifest position")
		}
	}
	if err := p.Network.Validate(); err != nil {
		return fmt.Errorf("launch network: %w", err)
	}
	return nil
}

// OverlayV1Config contains values rendered into the overlay-v1 guest boot
// contract. Grouping them keeps future boot parameters explicit.
type OverlayV1Config struct {
	// LayerCount is the number of immutable image disks.
	LayerCount int
	// Hostname is the validated sandbox name applied by early userspace.
	Hostname string
	// Interfaces contains persisted guest identities in eth index order.
	Interfaces []types.NetworkInterface
	// DNSServers supplies up to two IPv4 resolvers to static kernel IP entries.
	DNSServers []string
}

// OverlayV1Cmdline renders the public KumaBox boot ABI. Layer disks attach in
// base-to-top order, while OverlayFS lowerdirs must be listed top-to-base.
func OverlayV1Cmdline(config OverlayV1Config) (string, error) {
	if config.LayerCount <= 0 {
		return "", errors.New("overlay-v1 requires at least one image layer")
	}
	if config.Hostname == "" || strings.ContainsAny(config.Hostname, " \t\r\n\x00") {
		return "", errors.New("overlay-v1 requires a hostname without whitespace")
	}
	serials := make([]string, 0, config.LayerCount)
	for position := config.LayerCount - 1; position >= 0; position-- {
		serials = append(serials, fmt.Sprintf("%s%d", LayerSerialPrefix, position))
	}
	var commandLine strings.Builder
	commandLine.WriteString("console=hvc0 loglevel=3 boot=kumabox-overlay kumabox.layers=")
	commandLine.WriteString(strings.Join(serials, ","))
	commandLine.WriteString(" kumabox.cow=" + COWSerial + " kumabox.hostname=" + config.Hostname + " clocksource=kvm-clock rw")
	if len(config.Interfaces) == 0 {
		return commandLine.String(), nil
	}
	commandLine.WriteString(" net.ifnames=0")
	dns, err := ipv4DNSServers(config.DNSServers)
	if err != nil {
		return "", err
	}
	for _, networkInterface := range config.Interfaces {
		if err := networkInterface.Validate(); err != nil {
			return "", err
		}
		if networkInterface.IPv4 == nil {
			continue
		}
		mask := net.IP(net.CIDRMask(networkInterface.IPv4.Prefix, 32)).String()
		parameter := fmt.Sprintf(" ip=%s::%s:%s:%s:%s:off",
			networkInterface.IPv4.Address, networkInterface.IPv4.Gateway,
			mask, config.Hostname, networkInterface.Name,
		)
		if len(dns) > 0 {
			parameter += ":" + dns[0]
			if len(dns) > 1 {
				parameter += ":" + dns[1]
			}
		}
		commandLine.WriteString(parameter)
	}
	return commandLine.String(), nil
}

func ipv4DNSServers(configured []string) ([]string, error) {
	result := make([]string, 0, min(2, len(configured)))
	for _, server := range configured {
		address := net.ParseIP(server)
		if address == nil || address.To4() == nil {
			return nil, fmt.Errorf("overlay-v1 DNS server %q is not IPv4", server)
		}
		if len(result) < 2 {
			result = append(result, server)
		}
	}
	return result, nil
}

// Process identifies one Linux process generation independently of PID reuse.
type Process struct {
	// PID is the host process ID observed immediately after launch.
	PID int `json:"pid"`
	// StartTicks is Linux /proc stat starttime for this PID generation.
	StartTicks uint64 `json:"start_ticks"`
	// BootID invalidates all process identities after a host reboot.
	BootID string `json:"boot_id"`
	// SandboxID binds the process to one managed runtime directory.
	SandboxID types.SandboxID `json:"sandbox_id"`
	// Generation is the Starting catalog generation that launched the process.
	Generation uint64 `json:"generation"`
	// Binary is the executable basename required during process verification.
	Binary string `json:"binary"`
	// APISocket is the exact unique argument required during process verification.
	APISocket string `json:"api_socket"`
}

// Validate rejects identities that cannot safely authorize observation or signals.
func (p Process) Validate() error {
	if p.PID <= 0 || p.StartTicks == 0 || p.BootID == "" || p.Generation == 0 || p.Binary == "" || !filepath.IsAbs(p.APISocket) {
		return errors.New("process identity is incomplete")
	}
	if _, err := types.ParseSandboxID(p.SandboxID.String()); err != nil {
		return err
	}
	return nil
}

// ProcessState summarizes facts proven from process identity and the VMM API.
type ProcessState string

const (
	// ProcessAbsent means no owned VMM process is alive.
	ProcessAbsent ProcessState = "absent"
	// ProcessStarting means the owned process exists but its API is not Running.
	ProcessStarting ProcessState = "starting"
	// ProcessRunning means both identity and vm.info report a running VM.
	ProcessRunning ProcessState = "running"
)

// Observation is one fail-closed runtime snapshot.
type Observation struct {
	// State is absent, starting, or running.
	State ProcessState
	// Process is populated for starting and running observations.
	Process Process
}
