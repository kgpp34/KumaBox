package cloudhypervisor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const (
	memoryModeCopyOnWrite = "CopyOnWrite"
	memoryModeOnDemand    = "OnDemand"
)

var versionPattern = regexp.MustCompile(`\bv([0-9]+)\.`)

// cloneMemoryMode selects the fastest restore supported by both the captured
// memory backing and the installed monitor. Unknown versions use its default
// eager copy rather than submitting an unsupported API value.
func (d *Driver) cloneMemoryMode(ctx context.Context, configPath string) string {
	raw, err := os.ReadFile(configPath) //nolint:gosec // private managed snapshot copy
	if err != nil {
		return ""
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
	if json.Unmarshal(raw, &config) != nil || config.Memory.Size <= 0 || config.Memory.Shared || config.Memory.HugePages {
		return ""
	}
	for _, zone := range config.Memory.Zones {
		if zone.Shared || zone.HugePages {
			return ""
		}
	}
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, d.binary, "--version").Output() //nolint:gosec // configured monitor executable
	if err != nil {
		return ""
	}
	return memoryModeForVersion(string(output))
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
