package runtime

import (
	"context"
	"fmt"
	"time"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	"github.com/kumabox/kumabox/internal/agent/protocol"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	guestReseedTimeout        = 15 * time.Second
	guestReseedAttemptTimeout = 5 * time.Second
)

var reseedRestoredGuest = reseedGuest

// ReseedGuestVM injects fresh entropy into a running guest. Machine identity
// regeneration is intended for clones, not an in-place restore of the same VM.
func (r *Runtime) ReseedGuestVM(ctx context.Context, ref string, regenerateMachineID bool) (*vmstore.VMRecord, error) {
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for reseed: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	observed := r.applyObservation(rec)
	if observed.ObservedState != vmstore.ObservedStateRunning {
		return nil, fmt.Errorf("VM_NOT_RUNNING: VM %s is not running", rec.Name)
	}
	if err := reseedGuest(ctx, rec.VsockSocket, regenerateMachineID); err != nil {
		return nil, err
	}
	return observed, nil
}

func reseedGuest(ctx context.Context, socket string, regenerateMachineID bool) error {
	if socket == "" {
		return fmt.Errorf("AGENT_NOT_READY: VM has no guest agent vsock socket")
	}
	reseedCtx, cancel := context.WithTimeout(ctx, guestReseedTimeout)
	defer cancel()
	pong, err := agentclient.Ping(reseedCtx, socket)
	if err != nil {
		return fmt.Errorf("wait for guest agent reseed: %w", err)
	}
	if err := requireAgentCapability(pong, agentclient.CapabilityReseed); err != nil {
		return err
	}
	attemptCtx, attemptCancel := context.WithTimeout(reseedCtx, guestReseedAttemptTimeout)
	defer attemptCancel()
	if _, err := agentclient.Reseed(attemptCtx, socket, regenerateMachineID); err != nil {
		return fmt.Errorf("reseed guest: %w", err)
	}
	return nil
}

func requireAgentCapability(pong *agentclient.PingPongResponse, capability protocol.Capability) error {
	if pong.Supports(capability) {
		return nil
	}
	if pong == nil {
		return fmt.Errorf("AGENT_CAPABILITY_MISSING: guest agent response is empty; required capability %q", capability)
	}
	version := pong.Version
	if version == "" {
		version = "unknown"
	}
	return fmt.Errorf(
		"AGENT_CAPABILITY_MISSING: guest agent %s does not advertise %q (capabilities=%v); rebuild the managed image with the current kumabox-agent",
		version, capability, pong.Capabilities,
	)
}
