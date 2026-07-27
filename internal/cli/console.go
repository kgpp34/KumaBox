package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newConsoleCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "console VM",
		Short: "Connect to a running VM console",
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
			stream, err := rt.OpenConsole(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer stream.Close() //nolint:errcheck
			if isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout()) {
				return relayInteractiveConsole(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), stream)
			}
			return relayConsole(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), stream)
		},
	}
}

type consoleSizer interface {
	SetSize(uint16, uint16) error
}

func isTerminal(value any) bool {
	file, ok := value.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func relayInteractiveConsole(ctx context.Context, in io.Reader, out io.Writer, stream io.ReadWriteCloser) error {
	input, ok := in.(*os.File)
	if !ok {
		return fmt.Errorf("interactive console requires terminal stdin")
	}
	columns, rows, err := term.GetSize(int(input.Fd()))
	if err != nil {
		return fmt.Errorf("get terminal size: %w", err)
	}
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	defer term.Restore(int(input.Fd()), state) //nolint:errcheck
	setRemoteConsoleSize(stream, uint16(rows), uint16(columns))

	resizeSignal := make(chan os.Signal, 1)
	signal.Notify(resizeSignal, syscall.SIGWINCH)
	defer signal.Stop(resizeSignal)
	inputData := make(chan []byte, 1)
	inputErr := make(chan error, 1)
	readInput := func() {
		buffer := make([]byte, 32*1024)
		n, readErr := input.Read(buffer)
		if n > 0 {
			inputData <- append([]byte(nil), buffer[:n]...)
		}
		inputErr <- readErr
	}
	go readInput()

	relayErrs := make(chan error, 1)
	go func() { _, copyErr := io.Copy(out, stream); relayErrs <- copyErr }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resizeSignal:
			width, height, sizeErr := term.GetSize(int(input.Fd()))
			if sizeErr == nil {
				setRemoteConsoleSize(stream, uint16(height), uint16(width))
			}
		case data := <-inputData:
			if index := indexConsoleEscape(data); index >= 0 {
				if index > 0 {
					if _, err := stream.Write(data[:index]); err != nil {
						return fmt.Errorf("write console input: %w", err)
					}
				}
				return nil
			}
			if _, err := stream.Write(data); err != nil {
				return fmt.Errorf("write console input: %w", err)
			}
			go readInput()
		case readErr := <-inputErr:
			if readErr == nil {
				continue
			}
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("read console input: %w", readErr)
		case relayErr := <-relayErrs:
			if relayErr == nil || relayErr == io.EOF {
				return nil
			}
			return fmt.Errorf("relay console: %w", relayErr)
		}
	}
}

func setRemoteConsoleSize(stream io.ReadWriteCloser, rows, columns uint16) {
	if sizer, ok := stream.(consoleSizer); ok {
		_ = sizer.SetSize(rows, columns)
	}
}

func indexConsoleEscape(data []byte) int {
	for index, value := range data {
		if value == 0x1d { // Ctrl-] is the console escape sequence.
			return index
		}
	}
	return -1
}

func relayConsole(ctx context.Context, in io.Reader, out io.Writer, stream io.ReadWriteCloser) error {
	errCh := make(chan error, 2)
	go func() { _, err := io.Copy(out, stream); errCh <- err }()
	go func() { _, err := io.Copy(stream, in); errCh <- err }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == nil || err == io.EOF {
			return nil
		}
		return fmt.Errorf("relay console: %w", err)
	}
}
