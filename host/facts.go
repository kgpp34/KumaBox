// Package host describes what this machine can do and whether it is ready for
// the phase being worked on (docs/HOST.md).
//
// Collecting facts reads the machine; evaluating them is a pure function, so
// every judgement can be tested on any platform, including a development laptop
// with no KVM.
package host

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotReady reports that this machine cannot run the phase being checked.
var ErrNotReady = errors.New("machine is not ready")

// Phase is a delivery phase from docs/ROADMAP.md. A requirement records the
// phase from which it becomes mandatory, so doctor can tell "not ready" apart
// from "not needed yet".
type Phase uint8

// Delivery phases, in order.
const (
	PhaseImage Phase = iota
	PhaseSandbox
	PhaseNetwork
	PhaseSnapshot
	PhaseClone
	PhaseConvergence
	PhaseCrossNode
	PhaseProduction
)

func (p Phase) String() string {
	if p > PhaseProduction {
		return "unknown"
	}
	return fmt.Sprintf("S%d", int(p)+1)
}

// MarshalText renders the phase as "S1".."S8" so a JSON report is readable by a
// control plane instead of a bare integer.
func (p Phase) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// State is the outcome of one check, per docs/HOST.md §3.
type State uint8

// Check outcomes.
const (
	StateOK State = iota
	StateMissing
	StateUnsupported
	StateNotRequired
)

func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateMissing:
		return "missing"
	case StateUnsupported:
		return "unsupported"
	case StateNotRequired:
		return "not-required"
	default:
		return "unknown"
	}
}

// MarshalText renders the state as text, never as an integer.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Facts is a snapshot of what this machine looks like right now.
type Facts struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel"`
	Root     Root   `json:"root"`
	KVM      KVM    `json:"kvm"`
	VMM      Binary `json:"cloud_hypervisor"`
	CNI      CNI    `json:"cni"`
	CgroupV2 bool   `json:"cgroup_v2"`
}

// Root describes the node root directory.
type Root struct {
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Writable   bool   `json:"writable"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// KVM describes the virtualization device.
type KVM struct {
	Device   string `json:"device"`
	Present  bool   `json:"present"`
	Readable bool   `json:"readable"`
}

// Binary describes an external executable the runtime depends on.
type Binary struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Found   bool   `json:"found"`
}

// CNI describes the CNI plugin and configuration directories.
type CNI struct {
	PluginBinDir string `json:"plugin_bin_dir"`
	ConfigDir    string `json:"config_dir"`
	PluginsFound bool   `json:"plugins_found"`
	ConfigFound  bool   `json:"config_found"`
}

// Check is the evaluation of one requirement.
type Check struct {
	Name   string `json:"name"`
	State  State  `json:"state"`
	Since  Phase  `json:"since"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the full doctor result.
type Report struct {
	Phase  Phase   `json:"phase"`
	OS     string  `json:"os"`
	Arch   string  `json:"arch"`
	Kernel string  `json:"kernel"`
	Root   string  `json:"root"`
	Checks []Check `json:"checks"`
}

// Failed reports whether a requirement of the current phase is unmet. Checks
// that belong to a later phase never make a run fail.
func (r Report) Failed() bool {
	for _, check := range r.Checks {
		if check.State == StateMissing || check.State == StateUnsupported {
			return true
		}
	}
	return false
}

// minCHMajor is the oldest Cloud Hypervisor KumaBox supports. It is a
// placeholder until S2 pins the version against a real deployment.
const minCHMajor = 43

// minFreeBytes is the free space a node root must have before it counts as
// usable. It is deliberately generous until S2 measures real image sizes.
const minFreeBytes = uint64(10) << 30

// Evaluate judges facts against the requirements of the given phase.
func Evaluate(facts Facts, phase Phase) Report {
	return Report{
		Phase:  phase,
		OS:     facts.OS,
		Arch:   facts.Arch,
		Kernel: facts.Kernel,
		Root:   facts.Root.Path,
		Checks: []Check{
			rootWritable(facts, phase),
			rootSpace(facts, phase),
			linuxKernel(facts, phase),
			kvmAvailable(facts, phase),
			cloudHypervisor(facts, phase),
			cniAvailable(facts, phase),
			cgroupV2(facts, phase),
		},
	}
}

func rootWritable(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "root-directory",
		Since: PhaseImage,
		Fix:   fmt.Sprintf("kumabox doctor --fix --root %s", facts.Root.Path),
	}
	switch {
	case facts.Root.Path == "":
		check.State = StateUnsupported
		check.Detail = "no root directory was resolved"
	case !facts.Root.Exists:
		check.State = StateMissing
		check.Detail = fmt.Sprintf("%s does not exist yet", facts.Root.Path)
	case !facts.Root.Writable:
		check.State = StateMissing
		check.Detail = fmt.Sprintf("%s exists but is not writable", facts.Root.Path)
	default:
		check.State = StateOK
		check.Detail = fmt.Sprintf("%s is writable", facts.Root.Path)
		check.Fix = ""
	}
	return withPhase(check, phase)
}

func rootSpace(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "root-space",
		Since: PhaseImage,
		Fix:   "free space on the volume that holds the node root",
	}
	switch {
	case facts.Root.FreeBytes == 0:
		check.State = StateMissing
		check.Detail = "free space could not be determined"
	case facts.Root.FreeBytes < minFreeBytes:
		check.State = StateMissing
		check.Detail = fmt.Sprintf("only %s free, want at least %s",
			HumanBytes(facts.Root.FreeBytes), HumanBytes(minFreeBytes))
	default:
		check.State = StateOK
		check.Detail = fmt.Sprintf("%s free of %s",
			HumanBytes(facts.Root.FreeBytes), HumanBytes(facts.Root.TotalBytes))
		check.Fix = ""
	}
	return withPhase(check, phase)
}

func linuxKernel(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "linux-kernel",
		Since: PhaseSandbox,
		Fix:   "run KumaBox on Linux; microVMs need KVM and cgroup v2",
	}
	if facts.OS == "linux" {
		check.State = StateOK
		check.Detail = fmt.Sprintf("linux %s %s", facts.Kernel, facts.Arch)
		check.Fix = ""
	} else {
		check.State = StateUnsupported
		check.Detail = fmt.Sprintf("%s/%s cannot boot microVMs", facts.OS, facts.Arch)
	}
	return withPhase(check, phase)
}

func kvmAvailable(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "kvm",
		Since: PhaseSandbox,
		Fix:   "load the kvm module and make " + facts.KVM.Device + " readable by the KumaBox user",
	}
	switch {
	case facts.KVM.Present && facts.KVM.Readable:
		check.State = StateOK
		check.Detail = facts.KVM.Device + " is readable"
		check.Fix = ""
	case facts.KVM.Present:
		check.State = StateMissing
		check.Detail = facts.KVM.Device + " exists but is not readable"
	case facts.OS != "linux":
		check.State = StateUnsupported
		check.Detail = "KVM requires Linux"
	default:
		check.State = StateMissing
		check.Detail = facts.KVM.Device + " does not exist"
	}
	return withPhase(check, phase)
}

func cloudHypervisor(facts Facts, phase Phase) Check {
	required := fmt.Sprintf("v%d.0", minCHMajor)
	check := Check{
		Name:  "cloud-hypervisor",
		Since: PhaseSandbox,
		Fix:   "install Cloud Hypervisor " + required + " or newer (see hack/host-install.sh)",
	}
	switch {
	case !facts.VMM.Found:
		check.State = StateMissing
		check.Detail = fmt.Sprintf("%s not found in PATH", facts.VMM.Name)
	case MajorOf(facts.VMM.Version) < minCHMajor:
		check.State = StateMissing
		check.Detail = fmt.Sprintf("%s is older than %s", facts.VMM.Version, required)
	default:
		check.State = StateOK
		check.Detail = fmt.Sprintf("%s at %s", facts.VMM.Version, facts.VMM.Path)
		check.Fix = ""
	}
	return withPhase(check, phase)
}

func cniAvailable(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "cni",
		Since: PhaseNetwork,
		Fix: "install CNI plugins into " + facts.CNI.PluginBinDir +
			" and add a conflist to " + facts.CNI.ConfigDir,
	}
	switch {
	case facts.CNI.PluginsFound && facts.CNI.ConfigFound:
		check.State = StateOK
		check.Detail = fmt.Sprintf("plugins in %s, config in %s",
			facts.CNI.PluginBinDir, facts.CNI.ConfigDir)
		check.Fix = ""
	case !facts.CNI.PluginsFound:
		check.State = StateMissing
		check.Detail = facts.CNI.PluginBinDir + " has no plugins"
	default:
		check.State = StateMissing
		check.Detail = facts.CNI.ConfigDir + " has no network configuration"
	}
	return withPhase(check, phase)
}

func cgroupV2(facts Facts, phase Phase) Check {
	check := Check{
		Name:  "cgroup-v2",
		Since: PhaseConvergence,
		Fix:   "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)",
	}
	switch {
	case facts.CgroupV2:
		check.State = StateOK
		check.Detail = "unified hierarchy is mounted"
		check.Fix = ""
	case facts.OS != "linux":
		check.State = StateUnsupported
		check.Detail = "cgroup v2 requires Linux"
	default:
		check.State = StateMissing
		check.Detail = "/sys/fs/cgroup/cgroup.controllers is missing"
	}
	return withPhase(check, phase)
}

// withPhase downgrades a failure to StateNotRequired when the requirement
// belongs to a later phase than the one being checked.
func withPhase(check Check, phase Phase) Check {
	if check.Since > phase && (check.State == StateMissing || check.State == StateUnsupported) {
		check.State = StateNotRequired
		check.Detail = fmt.Sprintf("%s (needed from %s)", check.Detail, check.Since)
		check.Fix = ""
	}
	return check
}

// MajorOf extracts the major version from strings such as "Cloud Hypervisor
// v43.0" or "43.0". It reports 0 when no version can be read.
func MajorOf(version string) int {
	for i := 0; i < len(version); i++ {
		if version[i] < '0' || version[i] > '9' {
			continue
		}
		major := 0
		for ; i < len(version) && version[i] >= '0' && version[i] <= '9'; i++ {
			major = major*10 + int(version[i]-'0')
		}
		return major
	}
	return 0
}

// HumanBytes renders a byte count the way an operator reads it.
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + suffix
		}
	}
	return fmt.Sprintf("%.1f EiB", value/unit)
}
