//go:build !linux

package agent

import (
	"context"
	"errors"
)

func applyGuestNetwork(context.Context, NetworkConfig) error {
	return errors.New("guest network configuration requires Linux")
}
