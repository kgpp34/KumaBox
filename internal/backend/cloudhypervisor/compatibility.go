package cloudhypervisor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const nativeSnapshotFormat = "cloud-hypervisor-native-v1"

func (Backend) InspectNativeHost(ctx context.Context, rec *vmstore.VMRecord) (backend.NativeHost, error) {
	if rec == nil {
		return backend.NativeHost{}, fmt.Errorf("VM record is nil")
	}
	cfg, err := readRenderedConfig(rec.Config)
	if err != nil {
		return backend.NativeHost{}, fmt.Errorf("read backend config: %w", err)
	}
	output, err := exec.CommandContext(ctx, cfg.Binary, "--version").CombinedOutput() //nolint:gosec
	if err != nil {
		return backend.NativeHost{}, fmt.Errorf("inspect cloud-hypervisor version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	vendor, features := linuxCPUIdentity()
	return backend.NativeHost{
		BackendName: "cloud-hypervisor", BackendVersion: parseBackendVersion(string(output)),
		SnapshotFormat: nativeSnapshotFormat, Architecture: runtime.GOARCH,
		CPUVendor: vendor, CPUFeatures: features,
	}, nil
}

func parseBackendVersion(output string) string {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return "unknown"
	}
	return strings.TrimPrefix(fields[len(fields)-1], "v")
}

func linuxCPUIdentity() (string, []string) {
	file, err := os.Open("/proc/cpuinfo") //nolint:gosec
	if err != nil {
		return "unknown", nil
	}
	defer file.Close() //nolint:errcheck

	vendor := "unknown"
	var features []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "vendor_id":
			vendor = strings.TrimSpace(value)
		case "flags":
			features = strings.Fields(value)
		}
		if vendor != "unknown" && len(features) > 0 {
			break
		}
	}
	sort.Strings(features)
	return vendor, features
}
