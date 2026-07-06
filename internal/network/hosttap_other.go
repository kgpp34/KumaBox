//go:build !linux

package network

import (
	"context"
	"fmt"
	"runtime"

	"github.com/kumabox/kumabox/internal/config"
)

func EnsureHostTap(_ context.Context, _ string, _ config.NetworkConfig) (*HostTapReport, error) {
	return nil, fmt.Errorf("host-tap networking requires Linux (running on %s)", runtime.GOOS)
}

func TeardownHostTap(_ context.Context, _ string, _ config.NetworkConfig) (*HostTapReport, error) {
	return nil, fmt.Errorf("host-tap networking requires Linux (running on %s)", runtime.GOOS)
}
