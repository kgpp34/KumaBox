//go:build !linux

package cgroup

import (
	"context"
	"errors"
	"os"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

func unsupported() error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("VMM cgroups require Linux cgroup v2"))
}

// Prepare rejects VMM launch on non-Linux development hosts.
func (*Manager) Prepare(context.Context, types.SandboxID, uint32, types.CPUPolicy) (*os.File, error) {
	return nil, unsupported()
}

// PrepareRestore rejects snapshot provisioning on non-Linux hosts.
func (*Manager) PrepareRestore(context.Context, types.SandboxID, uint32, types.CPUPolicy) (*os.File, error) {
	return nil, unsupported()
}

// ApplyLimit rejects runtime CPU policy changes on non-Linux hosts.
func (*Manager) ApplyLimit(context.Context, types.SandboxID, uint32, types.CPUPolicy) error {
	return unsupported()
}

// PIDs rejects process ownership inspection on non-Linux hosts.
func (*Manager) PIDs(types.SandboxID) ([]int, error) { return nil, unsupported() }

// Remove has no non-Linux scope to reclaim.
func (*Manager) Remove(context.Context, types.SandboxID) error { return unsupported() }
