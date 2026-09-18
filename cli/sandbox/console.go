package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/term"
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

const defaultConsoleEscape = "^]"

// NewConsoleCommand builds the interactive direct-boot console command.
func NewConsoleCommand(roots rootsProvider) *cobra.Command {
	escapeText := defaultConsoleEscape
	command := &cobra.Command{
		Use:   "console SANDBOX",
		Short: "attach to a running sandbox console",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			escape, err := parseEscapeChar(escapeText)
			if err != nil {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--escape-char: %w", err))
			}
			input, ok := command.InOrStdin().(*os.File)
			if !ok || !term.IsTerminal(input.Fd()) {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("console stdin must be a terminal"))
			}

			reference := args[0]
			service, err := core.OpenSandbox(command.Context(), roots(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "console sandbox", reference, "close metadata", "retry the console connection", false))
			}()
			connection, err := service.Console(command.Context(), reference)
			if err != nil {
				return err
			}
			defer func() {
				if connection != nil {
					returnErr = errors.Join(returnErr, connection.Close())
				}
			}()

			state, err := term.SetRawTerminal(input.Fd())
			if err != nil {
				return fmt.Errorf("set console terminal raw mode: %w", err)
			}
			defer func() {
				restoreErr := term.RestoreTerminal(input.Fd(), state)
				_, reportErr := fmt.Fprintf(command.ErrOrStderr(), "\r\nDisconnected from %s.\r\n", reference)
				returnErr = errors.Join(returnErr, restoreErr, reportErr)
			}()

			if _, err := fmt.Fprintf(command.ErrOrStderr(), "Connected to %s (escape sequence: %s.).\r\n", reference, formatEscapeChar(escape)); err != nil {
				return err
			}
			if remote, ok := connection.(*os.File); ok {
				stopResize := relayConsoleResize(input.Fd(), remote.Fd())
				defer stopResize()
			}
			err = relayConsole(command.Context(), connection, input, command.OutOrStdout(), []byte{escape, '.'})
			connection = nil // relayConsole closes the connection on every return path.
			return err
		},
	}
	command.Flags().StringVar(&escapeText, "escape-char", defaultConsoleEscape, "detach escape character (single byte or ^X notation; press it then .)")
	return command
}

// relayConsole copies both directions until the remote closes, the caller is
// canceled, or the local escape sequence detaches. It never closes stdin.
func relayConsole(ctx context.Context, remote io.ReadWriteCloser, input io.Reader, output io.Writer, escape []byte) error {
	errorsOut := make(chan error, 2)
	done := make(chan struct{})
	go func() {
		_, err := io.Copy(output, remote)
		errorsOut <- err
	}()
	go func() {
		reader := input
		if len(escape) != 0 {
			reader = term.NewEscapeProxy(input, escape)
		}
		_, err := io.Copy(remote, reader)
		errorsOut <- err
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = remote.Close()
		case <-done:
		}
	}()

	err := <-errorsOut
	close(done)
	closeErr := remote.Close()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, closeErr)
	}
	if cleanConsoleExit(err) {
		return nil
	}
	return errors.Join(err, closeErr)
}

// relayConsoleResize copies the local terminal size initially and on SIGWINCH.
func relayConsoleResize(localFD, remoteFD uintptr) func() {
	syncSize := func() {
		if size, err := term.GetWinsize(localFD); err == nil {
			_ = term.SetWinsize(remoteFD, size)
		}
	}
	syncSize()

	resize := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(resize, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-resize:
				syncSize()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(resize)
		close(done)
	}
}

// parseEscapeChar accepts Cocoon-compatible caret notation or one ASCII byte.
func parseEscapeChar(value string) (byte, error) {
	if len(value) == 2 && value[0] == '^' {
		char := value[1]
		switch {
		case char >= '@' && char <= '_':
			return validateEscapeChar(char - '@')
		case char >= 'a' && char <= 'z':
			return validateEscapeChar(char - 'a' + 1)
		default:
			return 0, fmt.Errorf("invalid caret notation %q; use ^A through ^_", value)
		}
	}
	if len(value) != 1 {
		return 0, fmt.Errorf("expected one byte or ^X notation, got %q", value)
	}
	return validateEscapeChar(value[0])
}

func validateEscapeChar(value byte) (byte, error) {
	if value == 0 || value == '\r' || value == '\n' || value == 0x7f || value >= 0x80 {
		return 0, fmt.Errorf("byte 0x%02x cannot be used as an escape character", value)
	}
	return value, nil
}

func formatEscapeChar(value byte) string {
	if value >= 1 && value <= 0x1f {
		return "^" + string(rune(value+'@'))
	}
	return string(value)
}

func cleanConsoleExit(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var escaped term.EscapeError
	return errors.As(err, &escaped)
}
