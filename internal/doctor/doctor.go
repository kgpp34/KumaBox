package doctor

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/kumabox/kumabox/internal/config"
)

const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
)

type Report struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

type Check struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message"`
	SuggestedAction string `json:"suggestedAction,omitempty"`
}

func Run(cfg config.Config) Report {
	checks := []Check{
		checkPaths(cfg),
		checkKVM(),
		checkCloudHypervisor(cfg),
	}
	return Report{
		Status: overallStatus(checks),
		Checks: checks,
	}
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
