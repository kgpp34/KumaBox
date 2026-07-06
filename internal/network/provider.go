package network

import (
	"fmt"

	"github.com/kumabox/kumabox/internal/config"
)

func ResolveProvider(cfg config.NetworkConfig) (string, error) {
	switch cfg.Mode {
	case ProviderNone, ProviderHostTap, ProviderCNI:
		return cfg.Mode, nil
	default:
		return "", fmt.Errorf("NETWORK_PROVIDER_NOT_CONFIGURED: unsupported network mode %q", cfg.Mode)
	}
}
