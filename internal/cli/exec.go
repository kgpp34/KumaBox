package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newExecCommand(opts *rootOptions) *cobra.Command {
	var env []string
	var workdir string
	var timeout time.Duration
	var jsonOutput bool
	var interactive bool
	var tty bool

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
			stdin := cmd.InOrStdin()
			if tty && !interactive {
				interactive = true
			}
			if !interactive {
				stdin, err = optionalStdin(stdin)
				if err != nil {
					return err
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			if tty {
				if jsonOutput {
					return fmt.Errorf("--json cannot be combined with --tty")
				}
				return runTTYExec(ctx, cmd, rec.VsockSocket, agentclient.ExecRequest{
					Args: args[1:], Env: env, WorkDir: workdir,
				}, stdin)
			}
			var stdout, stderr bytes.Buffer
			outWriter, errWriter := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if jsonOutput {
				outWriter, errWriter = &stdout, &stderr
			}
			code, err := agentclient.ExecStream(ctx, rec.VsockSocket, agentclient.ExecRequest{
				Args:    args[1:],
				Env:     env,
				WorkDir: workdir,
			}, stdin, outWriter, errWriter)
			if err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), agentclient.ExecResponse{
					OK:       code == 0,
					ExitCode: code,
					Stdout:   stdout.Bytes(),
					Stderr:   stderr.Bytes(),
				}); err != nil {
					return err
				}
			}
			if code != 0 {
				return commandExitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "environment variable in KEY=VALUE form")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the guest")
	cmd.Flags().DurationVar(&timeout, "timeout", agentclient.DefaultPingTimeout, "agent exec timeout")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print exec result as JSON")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open for the guest command")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a guest terminal")
	return cmd
}

func runTTYExec(ctx context.Context, cmd *cobra.Command, socketPath string, req agentclient.ExecRequest, stdin io.Reader) error {
	in, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return fmt.Errorf("--tty requires a terminal on stdin")
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	if !ok || !term.IsTerminal(int(out.Fd())) {
		return fmt.Errorf("--tty requires a terminal on stdout")
	}
	columns, rows, err := term.GetSize(int(in.Fd()))
	if err != nil {
		return fmt.Errorf("get terminal size: %w", err)
	}
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	defer term.Restore(int(in.Fd()), state) //nolint:errcheck

	resizeSignal := make(chan os.Signal, 1)
	signal.Notify(resizeSignal, syscall.SIGWINCH)
	defer signal.Stop(resizeSignal)
	resize := make(chan agentclient.TTYSize, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resizeSignal:
				width, height, sizeErr := term.GetSize(int(in.Fd()))
				if sizeErr == nil {
					select {
					case resize <- agentclient.TTYSize{Rows: uint16(height), Columns: uint16(width)}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	signalInput := make(chan os.Signal, 4)
	signal.Notify(signalInput, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signalInput)
	signals := make(chan string, 4)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case received := <-signalInput:
				name := map[os.Signal]string{
					syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM",
					syscall.SIGHUP: "SIGHUP", syscall.SIGQUIT: "SIGQUIT",
				}[received]
				if name != "" {
					select {
					case signals <- name:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	code, err := agentclient.ExecTTY(ctx, socketPath, req, in, cmd.OutOrStdout(), agentclient.TTYOptions{
		Rows: uint16(rows), Columns: uint16(columns), Resize: resize, Signals: signals,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return commandExitError{code: code}
	}
	return nil
}

func optionalStdin(r io.Reader) (io.Reader, error) {
	if file, ok := r.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("stat stdin: %w", err)
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	return r, nil
}
