package cli

import (
	"context"
	"fmt"
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
	return cmd
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
			rt := kbruntime.New(cfg)
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
				VMID        string                     `json:"vmId"`
				VMName      string                     `json:"vmName"`
				VsockSocket string                     `json:"vsockSocket"`
				Agent       *agentclient.HelloResponse `json:"agent"`
				CheckedAt   time.Time                  `json:"checkedAt"`
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
