package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newAgentCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Interact with the guest agent",
	}
	cmd.AddCommand(newAgentPingCommand(opts))
	cmd.AddCommand(newAgentStatusCommand(opts))
	return cmd
}

type agentStatusView struct {
	VMID          string                        `json:"vmId"`
	VMName        string                        `json:"vmName"`
	VMState       vmstore.VMState               `json:"vmState"`
	ObservedState vmstore.ObservedState         `json:"observedState,omitempty"`
	Readiness     string                        `json:"readiness"`
	Ready         bool                          `json:"ready"`
	VsockSocket   string                        `json:"vsockSocket,omitempty"`
	Agent         *agentclient.PingPongResponse `json:"agent,omitempty"`
	Error         string                        `json:"error,omitempty"`
	Diagnostics   map[string]string             `json:"diagnostics,omitempty"`
	CheckedAt     time.Time                     `json:"checkedAt"`
}

func newAgentStatusCommand(opts *rootOptions) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "status VM",
		Short: "Inspect guest agent readiness and diagnostics",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.InspectVM(args[0])
			if err != nil {
				return err
			}
			view := inspectAgentStatus(cmd.Context(), rec, timeout)
			return writeJSON(cmd.OutOrStdout(), view)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", agentclient.DefaultPingTimeout, "agent readiness timeout")
	return cmd
}

func inspectAgentStatus(parent context.Context, rec *vmstore.VMRecord, timeout time.Duration) agentStatusView {
	view := agentStatusView{
		VMID:          rec.ID,
		VMName:        rec.Name,
		VMState:       rec.State,
		ObservedState: rec.ObservedState,
		Readiness:     "vm-not-running",
		CheckedAt:     time.Now().UTC(),
	}
	if rec.State != vmstore.StateRunning {
		view.Error = fmt.Sprintf("VM %s is not running", rec.Name)
		view.Diagnostics = guestDiagnostics(rec)
		return view
	}
	view.VsockSocket = rec.VsockSocket
	if rec.VsockSocket == "" {
		view.Readiness = "vsock-unavailable"
		view.Error = "VM has no guest agent vsock socket"
		view.Diagnostics = guestDiagnostics(rec)
		return view
	}
	if timeout <= 0 {
		timeout = agentclient.DefaultPingTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	pong, err := agentclient.Ping(ctx, rec.VsockSocket)
	if err != nil {
		view.Readiness = "agent-not-ready"
		view.Error = err.Error()
		view.Diagnostics = guestDiagnostics(rec)
		return view
	}
	view.Readiness = "ready"
	view.Ready = true
	view.Agent = pong
	return view
}

func guestDiagnostics(rec *vmstore.VMRecord) map[string]string {
	diagnostics := make(map[string]string, 2)
	for name, path := range map[string]string{
		"consoleTail":   filepath.Join(rec.LogDir, "console.log"),
		"vmmStderrTail": filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log"),
	} {
		if tail := readLogTail(path, 40); tail != "" {
			diagnostics[name] = tail
		}
	}
	if len(diagnostics) == 0 {
		return nil
	}
	return diagnostics
}

func readLogTail(path string, lines int) string {
	if path == "" || lines <= 0 {
		return ""
	}
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return ""
	}
	values := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(values) > lines {
		values = values[len(values)-lines:]
	}
	return strings.TrimSpace(strings.Join(values, "\n"))
}

func newAgentPingCommand(opts *rootOptions) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "ping VM",
		Short: "Check guest agent readiness",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.InspectVM(args[0])
			if err != nil {
				return err
			}
			if rec.State != vmstore.StateRunning {
				return fmt.Errorf("AGENT_NOT_READY: VM %s is not running", rec.Name)
			}
			if rec.VsockSocket == "" {
				return fmt.Errorf("AGENT_NOT_READY: VM %s has no vsock socket", rec.Name)
			}
			if timeout <= 0 {
				timeout = agentclient.DefaultPingTimeout
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			resp, err := agentclient.Ping(ctx, rec.VsockSocket)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), struct {
				VMID        string                        `json:"vmId"`
				VMName      string                        `json:"vmName"`
				VsockSocket string                        `json:"vsockSocket"`
				Agent       *agentclient.PingPongResponse `json:"agent"`
				CheckedAt   time.Time                     `json:"checkedAt"`
			}{
				VMID:        rec.ID,
				VMName:      rec.Name,
				VsockSocket: rec.VsockSocket,
				Agent:       resp,
				CheckedAt:   time.Now().UTC(),
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", agentclient.DefaultPingTimeout, "agent readiness timeout")
	return cmd
}
