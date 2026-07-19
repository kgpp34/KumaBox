package runtime

import (
	"context"
	"fmt"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func requiresAgentReadiness(rec *vmstore.VMRecord) bool {
	return rec != nil && rec.Image != nil && rec.Image.BootMode == "direct"
}

// verifyGuestExecReadiness is the final guest-side gate for restore and clone.
// A VMM process and a responding agent are not sufficient: the restored guest
// must also be able to complete an actual exec request before it is published
// as running.
func verifyGuestExecReadiness(ctx context.Context, socketPath string) error {
	readinessCtx, cancel := context.WithTimeout(ctx, agentclient.DefaultPingTimeout)
	defer cancel()
	hello, err := agentclient.Ping(readinessCtx, socketPath)
	if err != nil {
		return fmt.Errorf("wait for restored guest agent: %w", err)
	}
	if !hello.Supports(agentclient.CapabilityExec) {
		return fmt.Errorf("AGENT_CAPABILITY_MISSING: guest agent does not advertise %q", agentclient.CapabilityExec)
	}
	resp, err := agentclient.Exec(readinessCtx, socketPath, agentclient.ExecRequest{Args: []string{"true"}})
	if err != nil {
		return fmt.Errorf("verify restored guest exec: %w", err)
	}
	if !resp.OK || resp.ExitCode != 0 {
		return fmt.Errorf("verify restored guest exec: command exited with code %d", resp.ExitCode)
	}
	return nil
}
