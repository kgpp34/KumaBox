package host

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kumabox/kumabox/layout"
)

const (
	// KVMPath is the device a microVM needs.
	KVMPath = "/dev/kvm"
	// DefaultCNIPluginDir is where CNI plugins are installed.
	DefaultCNIPluginDir = "/opt/cni/bin"
	// DefaultCNIConfigDir is where CNI network configuration lives.
	DefaultCNIConfigDir = "/etc/cni/net.d"
	// cgroupV2Marker exists only when the unified cgroup hierarchy is mounted.
	cgroupV2Marker = "/sys/fs/cgroup/cgroup.controllers"

	// binaryTimeout bounds the version probe so doctor can never hang.
	binaryTimeout = 5 * time.Second
)

// Collector gathers facts about the machine.
//
// It only ever reads: no directory is created, no privileged operation is
// performed. Everything that needs root belongs to hack/host-install.sh
// (docs/HOST.md §4).
type Collector struct {
	// CNIPluginDir and CNIConfigDir are overridable for tests.
	CNIPluginDir string
	CNIConfigDir string
}

// Collect reports what this machine looks like right now.
func (c Collector) Collect(ctx context.Context, root layout.Root) (Facts, error) {
	facts := Facts{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Kernel:   kernelVersion(),
		KVM:      KVM{Device: KVMPath},
		VMM:      Binary{Name: "cloud-hypervisor"},
		CNI:      CNI{PluginBinDir: c.pluginDir(), ConfigDir: c.configDir()},
		CgroupV2: exists(cgroupV2Marker),
	}

	state, err := root.Inspect()
	if err != nil {
		return Facts{}, err
	}
	facts.Root = Root{
		Path:       root.Dir(),
		Exists:     state.Exists,
		Writable:   state.Writable,
		TotalBytes: state.TotalBytes,
		FreeBytes:  state.FreeBytes,
	}

	facts.KVM.Present, facts.KVM.Readable = kvmState()
	facts.VMM = binaryFacts(ctx, facts.VMM.Name)
	facts.CNI.PluginsFound = hasEntries(facts.CNI.PluginBinDir)
	facts.CNI.ConfigFound = hasEntries(facts.CNI.ConfigDir)
	return facts, nil
}

func (c Collector) pluginDir() string {
	if c.CNIPluginDir != "" {
		return c.CNIPluginDir
	}
	return DefaultCNIPluginDir
}

func (c Collector) configDir() string {
	if c.CNIConfigDir != "" {
		return c.CNIConfigDir
	}
	return DefaultCNIConfigDir
}

// kvmState reports whether the device exists and can be opened for read-write,
// which is what the runtime will need.
func kvmState() (present, readable bool) {
	info, err := os.Stat(KVMPath)
	if err != nil || info.IsDir() {
		return false, false
	}
	file, err := os.OpenFile(KVMPath, os.O_RDWR, 0)
	if err != nil {
		return true, false
	}
	_ = file.Close()
	return true, true
}

// binaryFacts looks up a tool and asks it for its version.
func binaryFacts(ctx context.Context, name string) Binary {
	binary := Binary{Name: name}
	path, err := exec.LookPath(name)
	if err != nil {
		return binary
	}
	binary.Path = path
	binary.Found = true

	probeCtx, cancel := context.WithTimeout(ctx, binaryTimeout)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, path, "--version").Output()
	if err != nil {
		return binary
	}
	binary.Version = firstLine(string(output))
	return binary
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}

// hasEntries reports whether dir exists and contains at least one entry.
func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
