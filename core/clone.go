package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// Clone creates a new running sandbox from an immutable native snapshot. It
// inherits the source resource shape while assigning a fresh identity, COW,
// network allocation, and VMM process. Source artifacts stay read-only.
//
//	snapshot lock -> validate -> Create -> private COW copy -> Starting
//	                                      -> rebind VMM -> guest network -> Running
func (s *SnapshotService) Clone(ctx context.Context, snapshotReference, name string) (result types.Sandbox, returnErr error) {
	if s == nil || s.lifecycle == nil || s.snapshots == nil || s.runtimes == nil || s.reporter == nil || s.now == nil {
		return types.Sandbox{}, errors.New("snapshot clone service is not configured")
	}
	if snapshotReference == "" || name == "" {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("SNAPSHOT and --name are required"))
	}
	if err := s.reporter.Status("resolving snapshot"); err != nil {
		return types.Sandbox{}, err
	}
	capture, err := s.snapshots.Resolve(ctx, snapshotReference)
	if err != nil {
		return types.Sandbox{}, err
	}
	lockPath, err := s.paths.Lock(capture.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return types.Sandbox{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	capture, err = s.snapshots.Resolve(ctx, capture.ID.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	config := capture.Config
	config.Name = name
	if err := config.Validate(); err != nil {
		return types.Sandbox{}, err
	}
	backend, err := s.runtimes.Backend(capture.VMM)
	if err != nil {
		return types.Sandbox{}, err
	}
	cloner, ok := backend.(vmm.Cloner)
	if !ok {
		return types.Sandbox{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM backend %q does not support clone", capture.VMM))
	}
	snapshotDir, err := s.paths.Dir(capture.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	snapshotCOW, err := s.paths.COW(capture.ID)
	if err != nil {
		return types.Sandbox{}, err
	}
	if info, err := os.Lstat(snapshotCOW); err != nil {
		return types.Sandbox{}, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	} else if !info.Mode().IsRegular() || info.Size() != config.Storage {
		return types.Sandbox{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("snapshot COW size or file type is invalid"))
	}
	if validator, ok := backend.(vmm.RestoreValidator); ok {
		if err := validator.ValidateRestore(ctx, snapshotDir); err != nil {
			return types.Sandbox{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
		}
	}
	if err := backend.Preflight(); err != nil {
		return types.Sandbox{}, err
	}
	if err := s.reporter.Status("creating clone identity and network"); err != nil {
		return types.Sandbox{}, err
	}
	created, err := s.lifecycle.Create(ctx, CreateSandboxRequest{
		ImageReference: capture.ImageDigest.String(), Config: config, VMM: capture.VMM,
		cloneDiskSource: snapshotCOW,
	})
	if err != nil {
		return created, err
	}
	result = created
	// Before Starting, any failure can cleanly release the newly created owner.
	cleanupCreated := true
	defer func() {
		if !cleanupCreated || returnErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, removeErr := s.lifecycle.Remove(cleanupCtx, created.ID.String())
		returnErr = errors.Join(returnErr, removeErr)
	}()
	sandboxLockPath, err := s.sandboxPaths.Lock(created.ID)
	if err != nil {
		return created, err
	}
	sandboxLock := filelock.New(sandboxLockPath)
	if err := sandboxLock.Lock(ctx); err != nil {
		return created, err
	}
	defer func() { returnErr = errors.Join(returnErr, sandboxLock.Unlock(context.WithoutCancel(ctx))) }()
	if err := s.reporter.Status("committing clone start"); err != nil {
		return created, err
	}
	starting, err := s.sandboxes.BeginStart(ctx, created.ID, created.Generation, s.now().UTC())
	if err != nil {
		return created, err
	}
	cleanupCreated = false
	result = starting
	if err := s.lifecycle.recoverNetwork(ctx, starting); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "recover network", err, vmm.Process{})
	}
	image, err := s.lifecycle.dependencies.images.WithAvailable(ctx, capture.ImageDigest.String(), func(types.Image) error { return nil })
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "resolve image", err, vmm.Process{})
	}
	launch, err := s.lifecycle.launchPlan(starting, image)
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "prepare local image layers", err, vmm.Process{})
	}
	liveCOW, err := s.sandboxPaths.COW(created.ID)
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "resolve disk", err, vmm.Process{})
	}
	if err := s.reporter.Status("restoring private VMM state"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, vmm.Process{})
	}
	process, err := cloner.Clone(ctx, vmm.ClonePlan{RestorePlan: vmm.RestorePlan{
		SandboxID: starting.ID, Generation: starting.Generation, CPUs: starting.Config.CPUs,
		SnapshotDir: snapshotDir, Network: starting.Network,
	}, WritableDisk: liveCOW, ImageDisks: launch.Disks[:len(launch.Disks)-1], Kernel: launch.Kernel, Initrd: launch.Initrd})
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "clone VMM", err, process)
	}
	if err := s.reporter.Status("configuring guest identity and network"); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "report", err, process)
	}
	if err := s.configureCloneGuest(ctx, backend, process, starting); err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "configure guest", err, process)
	}
	running, err := s.sandboxes.MarkRunning(ctx, starting.ID, starting.Generation, s.now().UTC())
	if err != nil {
		return starting, s.lifecycle.failStart(ctx, backend, starting, "commit running", err, process)
	}
	return running, nil
}

// configureCloneGuest applies the new MAC/IP map over vsock, which remains
// available even before the clone has a working guest network.
func (s *SnapshotService) configureCloneGuest(ctx context.Context, backend vmm.Backend, process vmm.Process, record types.Sandbox) error {
	script, err := cloneGuestScript(record, s.lifecycle.dependencies.dnsServers)
	if err != nil {
		return err
	}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		connection, err := backend.DialVsock(ctx, process, agent.Port)
		if err == nil {
			code, runErr := agent.Run(ctx, connection, types.Command{Args: []string{"/bin/sh", "-c", script}}, nil, nil, nil)
			_ = connection.Close()
			return errors.Join(runErr, guestExitError(code))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("guest agent unavailable after clone: %w", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func guestExitError(code int) error {
	if code != 0 {
		return fmt.Errorf("guest network configuration exited with status %d", code)
	}
	return nil
}

func cloneGuestScript(record types.Sandbox, dns []string) (string, error) {
	var script strings.Builder
	script.WriteString("set -eu\nmkdir -p /etc/systemd/network\nrm -f /etc/systemd/network/10-kumabox-*.network\n")
	for _, device := range record.Network.Interfaces {
		if err := device.Validate(); err != nil {
			return "", err
		}
		if device.IPv4 == nil {
			continue
		}
		filename := strings.ReplaceAll(device.MAC, ":", "")
		fmt.Fprintf(&script, "cat > /etc/systemd/network/10-kumabox-%s.network <<'KUMABOX_NETWORK'\n", filename)
		fmt.Fprintf(&script, "[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n", device.MAC, device.IPv4.Address, device.IPv4.Prefix)
		if device.IPv4.Gateway != "" {
			fmt.Fprintf(&script, "Gateway=%s\n", device.IPv4.Gateway)
		}
		for _, server := range dns {
			if ip := net.ParseIP(server); ip == nil || ip.To4() == nil {
				return "", fmt.Errorf("invalid guest DNS address %q", server)
			}
			fmt.Fprintf(&script, "DNS=%s\n", server)
		}
		script.WriteString("KUMABOX_NETWORK\n")
	}
	if err := record.Config.Validate(); err != nil {
		return "", err
	}
	fmt.Fprintf(&script, "printf '%%s\\n' '%s' > /etc/hostname\nhostname '%s'\n", record.Config.Name, record.Config.Name)
	if len(record.Network.Interfaces) > 0 {
		script.WriteString("systemctl restart systemd-networkd\n")
	}
	return script.String(), nil
}
