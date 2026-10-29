package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/daemon"
	"github.com/kumabox/kumabox/internal/errdefs"
)

// newDaemonCommand is the sole host-runtime owner for CLI and API requests.
func newDaemonCommand(configuration func() config.Config) *cobra.Command {
	options := daemon.Options{ReconcileInterval: 5 * time.Second, ShutdownTimeout: 30 * time.Second}
	command := &cobra.Command{
		Use: "daemon", Short: "serve the local runtime and supervise sandboxes", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if options.ReconcileInterval <= 0 || options.GCInterval < 0 || options.ShutdownTimeout <= 0 || options.MaxOperations <= 0 {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("daemon operation limit and intervals must be positive; GC interval may be zero"))
			}
			options.NotifySocket = config.NotifySocket()
			options.Ready = func(socket string) error {
				_, err := fmt.Fprintln(command.OutOrStdout(), "KumaBox daemon listening on "+socket)
				return err
			}
			return daemon.Run(command.Context(), configuration(), options, command.ErrOrStderr())
		},
	}
	command.Flags().DurationVar(&options.ReconcileInterval, "reconcile-interval", options.ReconcileInterval, "interval for lifecycle state repair")
	command.Flags().IntVar(&options.MaxOperations, "max-operations", 256, "maximum active requests, including streams")
	command.Flags().DurationVar(&options.GCInterval, "gc-interval", 0, "interval for orphan collection (0 disables periodic GC)")
	command.Flags().DurationVar(&options.ShutdownTimeout, "shutdown-timeout", options.ShutdownTimeout, "maximum graceful drain time before canceling requests")
	return command
}
