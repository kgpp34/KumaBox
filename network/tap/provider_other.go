//go:build !linux

package tap

import (
	"context"
	"errors"

	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
)

// ErrUnsupported reports that TAP/eBPF networking requires Linux.
var ErrUnsupported = errors.New("TAP/eBPF networking requires Linux")

func (*Provider) Prepare(context.Context, types.SandboxID) (string, error) { return "", ErrUnsupported }

func (*Provider) Add(context.Context, types.SandboxID, string, ...network.AddSpec) ([]types.NetworkInterface, error) {
	return nil, ErrUnsupported
}

func (*Provider) Verify(context.Context, types.SandboxID, []types.NetworkInterface) error {
	return ErrUnsupported
}

func (*Provider) Recover(context.Context, types.SandboxID, string, []types.NetworkInterface) ([]types.NetworkInterface, error) {
	return nil, ErrUnsupported
}
func (*Provider) Quiesce(context.Context, types.SandboxID) error   { return ErrUnsupported }
func (*Provider) Unquiesce(context.Context, types.SandboxID) error { return ErrUnsupported }
func (*Provider) Delete(context.Context, types.SandboxID) error    { return ErrUnsupported }
func (*Provider) Allocated(context.Context, types.SandboxID) ([]int, error) {
	return nil, ErrUnsupported
}
func (*Provider) Remove(context.Context, types.SandboxID, ...int) error { return ErrUnsupported }
func (*Provider) IndexForTAP(types.SandboxID, string) (int, bool)       { return 0, false }
func (*Provider) OwnedIDs(context.Context) ([]types.SandboxID, error)   { return nil, nil }
