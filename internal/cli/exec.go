package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newExecCommand(opts *rootOptions) *cobra.Command {
	var env []string
	var workdir string
	var timeout time.Duration
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "exec VM -- CMD [ARG...]",
		Short: "Execute a command inside a running guest",
		Args:  cobra.MinimumNArgs(2),
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
			stdin, err := readOptionalStdin(cmd.InOrStdin())
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			resp, err := agentclient.Exec(ctx, rec.VsockSocket, agentclient.ExecRequest{
				Args:    args[1:],
				Env:     env,
				WorkDir: workdir,
				Stdin:   stdin,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), resp); err != nil {
					return err
				}
			} else {
				if _, err := cmd.OutOrStdout().Write(resp.Stdout); err != nil {
					return err
				}
				if _, err := cmd.ErrOrStderr().Write(resp.Stderr); err != nil {
					return err
				}
			}
			if resp.ExitCode != 0 {
				return commandExitError{code: resp.ExitCode}
			}
			if !resp.OK {
				return fmt.Errorf("AGENT_EXEC_FAILED: %s", resp.Error)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "environment variable in KEY=VALUE form")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the guest")
	cmd.Flags().DurationVar(&timeout, "timeout", agentclient.DefaultPingTimeout, "agent exec timeout")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print exec result as JSON")
	return cmd
}

func readOptionalStdin(r io.Reader) ([]byte, error) {
	if file, ok := r.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat stdin: %w", err)
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	return raw, nil
}
