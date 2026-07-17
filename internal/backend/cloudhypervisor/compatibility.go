package cloudhypervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const nativeSnapshotFormat = "cloud-hypervisor-native-v1"

func (b Backend) InspectNativeHost(ctx context.Context, rec *vmstore.VMRecord) (backend.NativeHost, error) {
	binary := b.renderer.cfg.Backend.CloudHypervisor.Binary
	if rec != nil && rec.Config != "" {
		if cfg, err := readRenderedConfig(rec.Config); err == nil {
			binary = cfg.Binary
		}
	}
	if binary == "" {
		return backend.NativeHost{}, fmt.Errorf("Cloud Hypervisor binary is empty")
	}
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput() //nolint:gosec
	if err != nil {
		return backend.NativeHost{}, fmt.Errorf("inspect cloud-hypervisor version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	vendor, features := linuxCPUIdentity()
	return backend.NativeHost{
		BackendName: "cloud-hypervisor", BackendVersion: parseBackendVersion(string(output)),
		SnapshotFormat: nativeSnapshotFormat, Architecture: runtime.GOARCH,
		CPUVendor: vendor, CPUFeatures: features, RestoreModes: inspectRestoreModes(binary),
	}, nil
}

func inspectRestoreModes(binary string) []string {
	modes := []string{"copy"}
	path, err := exec.LookPath(binary)
	if err != nil {
		return modes
	}
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return modes
	}
	defer file.Close() //nolint:errcheck

	// Older builds ignore unknown restore JSON fields. Schema markers embedded
	// in the Rust binary let preflight fail before any destructive VM mutation.
	const overlap = 64
	buffer := make([]byte, 64<<10)
	window := make([]byte, 0, len(buffer)+overlap)
	var hasField, hasOnDemand, hasMmapSyntax bool
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			window = append(window, buffer[:n]...)
			hasField = hasField || bytes.Contains(window, []byte("memory_restore_mode"))
			hasOnDemand = hasOnDemand || bytes.Contains(window, []byte("OnDemand"))
			// "Mmap" appears in unrelated memory and device code in builds that
			// only accept Copy and OnDemand. Require the restore parser's exact
			// mode-list marker before advertising the optional mmap protocol.
			hasMmapSyntax = hasMmapSyntax || bytes.Contains(window, []byte("memory_restore_mode=copy|ondemand|mmap"))
			if len(window) > overlap {
				window = append(window[:0], window[len(window)-overlap:]...)
			}
			if hasField && hasOnDemand && hasMmapSyntax {
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return modes
			}
			break
		}
	}
	if hasField && hasOnDemand {
		modes = append(modes, "ondemand")
	}
	if hasField && hasMmapSyntax {
		modes = append(modes, "mmap")
	}
	return modes
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
