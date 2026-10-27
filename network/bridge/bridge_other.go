//go:build !linux

package bridge

import (
	"context"
	"errors"

	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
)

// ErrUnsupported reports that direct bridge networking requires Linux.
var ErrUnsupported = errors.New("bridge networking requires Linux")

// New constructs a provider that rejects every operation on non-Linux hosts.
func New(options Options) (*Provider, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Provider{prefix: options.TAPPrefix, queueSize: options.QueueSize, leaseDir: options.LeaseDir}, nil
}

// Add is unavailable without Linux network namespaces.
func (*Provider) Add(context.Context, types.SandboxID, string, ...network.AddSpec) ([]types.NetworkInterface, error) {
	return nil, ErrUnsupported
}

// Verify is unavailable without Linux network namespaces.
func (*Provider) Verify(context.Context, types.SandboxID, []types.NetworkInterface) error {
	return ErrUnsupported
}

// Recover is unavailable without Linux network namespaces.
func (*Provider) Recover(context.Context, types.SandboxID, string, []types.NetworkInterface) ([]types.NetworkInterface, error) {
	return nil, ErrUnsupported
}

// Delete is unavailable without Linux network namespaces.
func (*Provider) Delete(context.Context, types.SandboxID) error { return ErrUnsupported }
