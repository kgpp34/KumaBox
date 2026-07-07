// Package doctor runs host capability checks for KumaBox.
//
// Checks are deliberately descriptive rather than merely boolean because the
// Linux/KVM/network setup has several common failure modes that need actionable
// operator feedback.
package doctor

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

const (
	// StatusPass means the check succeeded.
	StatusPass = "pass"

	// StatusWarn means KumaBox can often proceed, but the operator may need
	// elevated permissions or a different environment.
	StatusWarn = "warn"

	// StatusFail means the checked capability is unavailable.
	StatusFail = "fail"
)

// Report groups all doctor checks with an aggregate status.
type Report struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// Check describes one host capability result.
type Check struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message"`
	SuggestedAction string `json:"suggestedAction,omitempty"`
}

// Run executes host, backend, and network capability checks.
//
// The function does not mutate host state. Setup commands such as network setup
// are responsible for making changes after the operator has reviewed failures.
func Run(cfg config.Config) Report {
	checks := []Check{
		checkPaths(cfg),
		checkKVM(),
		checkCloudHypervisor(cfg),
		checkNetworkProvider(cfg),
		checkNetworkTun(cfg),
		checkNetworkIPCommand(cfg),
		checkNetworkNAT(cfg),
		checkNetworkPermission(cfg),
	}
	return Report{
		Status: overallStatus(checks),
		Checks: checks,
	}
}

func checkNetworkProvider(cfg config.Config) Check {
	provider, err := kbnetwork.ResolveProvider(cfg.Network)
	if err != nil {
		return Check{
			Name:            "networkProvider",
			Status:          StatusFail,
			Code:            "NETWORK_PROVIDER_NOT_CONFIGURED",
			Message:         err.Error(),
			SuggestedAction: "set network.mode to host-tap, cni, or none",
		}
	}
	return Check{Name: "networkProvider", Status: StatusPass, Message: "network provider mode is " + provider}
}

func checkNetworkTun(cfg config.Config) Check {
	if cfg.Network.Mode == kbnetwork.ProviderNone {
		return Check{Name: "networkTun", Status: StatusPass, Message: "network disabled"}
	}
	if runtime.GOOS != "linux" {
		return Check{
			Name:            "networkTun",
			Status:          StatusFail,
			Code:            "NETWORK_TUN_UNAVAILABLE",
			Message:         "tuntap networking is only available on Linux",
			SuggestedAction: "run network-enabled KumaBox commands inside the Linux VM",
		}
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return Check{
			Name:            "networkTun",
			Status:          StatusFail,
			Code:            "TUNTAP_MISSING",
			Message:         err.Error(),
			SuggestedAction: "load the tun module and ensure /dev/net/tun exists",
		}
	}
	return Check{Name: "networkTun", Status: StatusPass, Message: "/dev/net/tun exists"}
}

func checkNetworkIPCommand(cfg config.Config) Check {
	if cfg.Network.Mode == kbnetwork.ProviderNone {
		return Check{Name: "networkIPCommand", Status: StatusPass, Message: "network disabled"}
	}
	path, err := exec.LookPath("ip")
	if err != nil {
		return Check{
			Name:            "networkIPCommand",
			Status:          StatusFail,
			Code:            "IPROUTE2_MISSING",
			Message:         "ip command not found",
			SuggestedAction: "install iproute2",
		}
	}
	return Check{Name: "networkIPCommand", Status: StatusPass, Message: "found " + path}
}

func checkNetworkNAT(cfg config.Config) Check {
	if cfg.Network.Mode == kbnetwork.ProviderNone || cfg.Network.NATBackend == "none" {
		return Check{Name: "networkNAT", Status: StatusPass, Message: "NAT disabled"}
	}
	iptablesPath, iptablesErr := exec.LookPath("iptables")
	nftPath, nftErr := exec.LookPath("nft")
	switch cfg.Network.NATBackend {
	case "iptables":
		if iptablesErr != nil {
			return Check{
				Name:            "networkNAT",
				Status:          StatusFail,
				Code:            "IPTABLES_MISSING",
				Message:         "iptables command not found",
				SuggestedAction: "install iptables or set network.nat_backend to nft",
			}
		}
		return Check{Name: "networkNAT", Status: StatusPass, Message: "found " + iptablesPath}
	case "nft":
		if nftErr != nil {
			return Check{
				Name:            "networkNAT",
				Status:          StatusFail,
				Code:            "NFT_MISSING",
				Message:         "nft command not found",
				SuggestedAction: "install nftables or set network.nat_backend to iptables",
			}
		}
		return Check{Name: "networkNAT", Status: StatusPass, Message: "found " + nftPath}
	default:
		if iptablesErr == nil {
			return Check{Name: "networkNAT", Status: StatusPass, Message: "found " + iptablesPath}
		}
		if nftErr == nil {
			return Check{Name: "networkNAT", Status: StatusPass, Message: "found " + nftPath}
		}
		return Check{
			Name:            "networkNAT",
			Status:          StatusFail,
			Code:            "NAT_BACKEND_MISSING",
			Message:         "neither iptables nor nft is available",
			SuggestedAction: "install iptables or nftables",
		}
	}
}

func checkNetworkPermission(cfg config.Config) Check {
	if cfg.Network.Mode == kbnetwork.ProviderNone {
		return Check{Name: "networkPermission", Status: StatusPass, Message: "network disabled"}
	}
	if runtime.GOOS != "linux" {
		return Check{
			Name:            "networkPermission",
			Status:          StatusFail,
			Code:            "NETWORK_PERMISSION_DENIED",
			Message:         "network setup requires Linux root privileges",
			SuggestedAction: "run network-enabled KumaBox commands as root or through sudo inside the Linux VM",
		}
	}
	if os.Geteuid() != 0 {
		return Check{
			Name:            "networkPermission",
			Status:          StatusFail,
			Code:            "NETWORK_PERMISSION_DENIED",
			Message:         "current user is not root",
			SuggestedAction: "run network-enabled KumaBox commands as root or through sudo",
		}
	}
	return Check{Name: "networkPermission", Status: StatusPass, Message: "current user can configure host networking"}
}

func checkPaths(cfg config.Config) Check {
	if err := config.EnsureRuntimeDirs(cfg); err != nil {
		return Check{
			Name:            "paths",
			Status:          StatusFail,
			Code:            "PATH_INIT_FAILED",
			Message:         err.Error(),
			SuggestedAction: "choose writable --root-dir, --run-dir and --log-dir paths",
		}
	}
	return Check{Name: "paths", Status: StatusPass, Message: "runtime directories are ready"}
}

func checkKVM() Check {
	if runtime.GOOS != "linux" {
		return Check{
			Name:            "kvm",
			Status:          StatusFail,
			Code:            "KVM_UNAVAILABLE",
			Message:         "KVM is only available on Linux",
			SuggestedAction: "run KumaBox inside the Linux VM with nested virtualization enabled",
		}
	}

	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return Check{
			Name:            "kvm",
			Status:          StatusFail,
			Code:            "KVM_UNAVAILABLE",
			Message:         err.Error(),
			SuggestedAction: "enable nested virtualization and ensure the current user can access /dev/kvm",
		}
	}
	_ = file.Close()
	return Check{Name: "kvm", Status: StatusPass, Message: "/dev/kvm is accessible"}
}

func checkCloudHypervisor(cfg config.Config) Check {
	binary := cfg.Backend.CloudHypervisor.Binary
	path, err := exec.LookPath(binary)
	if err != nil {
		return Check{
			Name:            "cloudHypervisor",
			Status:          StatusFail,
			Code:            "CH_MISSING",
			Message:         "cloud-hypervisor binary not found: " + binary,
			SuggestedAction: "install Cloud Hypervisor or set backend.cloud_hypervisor.binary",
		}
	}

	return Check{Name: "cloudHypervisor", Status: StatusPass, Message: "found " + path}
}

func overallStatus(checks []Check) string {
	for _, check := range checks {
		if check.Status == StatusFail {
			return StatusFail
		}
	}
	for _, check := range checks {
		if check.Status == StatusWarn {
			return StatusWarn
		}
	}
	return StatusPass
}
