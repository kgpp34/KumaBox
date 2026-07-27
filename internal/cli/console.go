package cli

import (
	"context"
	"fmt"
	"io"

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
			return relayConsole(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), stream)
		},
	}
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
