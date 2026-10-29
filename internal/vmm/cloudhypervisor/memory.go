package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
)

const (
	memoryModeCopyOnWrite = "CopyOnWrite"
	memoryModeOnDemand    = "OnDemand"
)

var versionPattern = regexp.MustCompile(`\bv([0-9]+)\.`)

// restoreMemoryMode applies one policy to in-place restore and clone. Automatic
// selection uses mmap when available, otherwise eager copy. Userfaultfd demand
// paging stays opt-in because first-touch work can worsen runtime tail latency;
// unsupported monitors fail explicit lazy requests during preflight. Shared or
// hugepage backing falls back from mmap to eager copy, preserving its backing.
func (d *Driver) restoreMemoryMode(ctx context.Context, configPath string, requested types.MemoryRestoreMode) (string, error) {
	if err := requested.Validate(); err != nil {
		return "", err
	}
	if requested == types.MemoryRestoreCopy {
		return "", nil // Eager copy is the monitor's backwards-compatible default.
	}
	raw, err := os.ReadFile(configPath) //nolint:gosec // private managed snapshot copy
	if err != nil {
		return "", err
	}
	var config struct {
		Memory struct {
			Size      int64 `json:"size"`
			Shared    bool  `json:"shared"`
			HugePages bool  `json:"hugepages"`
			Zones     []struct {
				Shared    bool `json:"shared"`
				HugePages bool `json:"hugepages"`
			} `json:"zones"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("read snapshot memory configuration: %w", err)
	}
	private := config.Memory.Size > 0 && !config.Memory.Shared && !config.Memory.HugePages
	for _, zone := range config.Memory.Zones {
		if zone.Shared || zone.HugePages {
			private = false
		}
	}
	if !private {
		if requested == types.MemoryRestoreMmap {
			return "", nil
		}
		if requested == types.MemoryRestoreOnDemand {
			return "", errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("lazy memory restore requires plain private guest memory; use --restore-mode copy"))
		}
		return "", nil
	}
	available, err := d.supportedMemoryMode(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if requested == types.MemoryRestoreMmap || requested == types.MemoryRestoreOnDemand {
			return "", errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("check memory restore support: %w", err))
		}
		return "", nil
	}
	switch requested {
	case types.MemoryRestoreMmap:
		if available == memoryModeCopyOnWrite {
			return memoryModeCopyOnWrite, nil
		}
	case types.MemoryRestoreOnDemand:
		if available != "" {
			return memoryModeOnDemand, nil
		}
	default:
		if available == memoryModeCopyOnWrite {
			return available, nil
		}
		return "", nil
	}
	return "", errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("monitor does not advertise support for %s restore; use --restore-mode copy or a compatible build", requested))
}

// supportedMemoryMode avoids executing the monitor repeatedly across source
// preflight, private clone validation, and restore. The configured executable
// is fixed for the lifetime of a driver; replacing it requires a new driver.
func (d *Driver) supportedMemoryMode(ctx context.Context) (string, error) {
	d.memorySupportMu.Lock()
	defer d.memorySupportMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if d.memorySupportKnown {
		return d.memorySupportMode, nil
	}
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, d.binary, "--version").Output() //nolint:gosec // configured monitor executable
	if err != nil {
		return "", err
	}
	d.memorySupportMode = memoryModeForVersion(string(output))
	d.memorySupportKnown = true
	return d.memorySupportMode, nil
}

func memoryModeForVersion(version string) string {
	match := versionPattern.FindStringSubmatch(version)
	if len(match) != 2 {
		return ""
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return ""
	}
	switch {
	case major >= 54:
		return memoryModeCopyOnWrite
	case major >= 53:
		return memoryModeOnDemand
	default:
		return ""
	}
}
