package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

func parseByteSize(value string) (int64, error) {
	return parsePositiveByteSize("--storage", value)
}

func parseMemorySize(value string) (int64, error) {
	return parsePositiveByteSize("--memory", value)
}

func parsePositiveByteSize(flag, value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s must not be empty", flag)
	}
	multiplier := int64(1)
	suffix := strings.ToUpper(trimmed[len(trimmed)-1:])
	switch suffix {
	case "K":
		multiplier = 1024
		trimmed = trimmed[:len(trimmed)-1]
	case "M":
		multiplier = 1024 * 1024
		trimmed = trimmed[:len(trimmed)-1]
	case "G":
		multiplier = 1024 * 1024 * 1024
		trimmed = trimmed[:len(trimmed)-1]
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive size like 512M or 4G", flag)
	}
	if n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%s exceeds the supported size", flag)
	}
	return n * multiplier, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func normalizedNetworkFlags(values []string) []string {
	if len(values) == 0 {
		return []string{"none"}
	}
	return append([]string(nil), values...)
}

func normalizedOCIImageNetworkFlags(values []string, cfg config.Config) []string {
	if len(values) > 0 {
		return append([]string(nil), values...)
	}
	if cfg.Network.Mode == kbnetwork.ProviderCNI {
		return []string{"cni:" + cfg.Network.Default}
	}
	return []string{"none"}
}
