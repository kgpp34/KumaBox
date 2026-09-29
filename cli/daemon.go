package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
)

// exitWatcher emits a hint when an identity-checked VMM process exits. The
// periodic reconciliation ticker remains the correctness floor.
type exitWatcher interface {
	Sync([]core.SandboxStatus) error
	Events() <-chan struct{}
	Close() error
}

// newDaemonCommand runs the same idempotent repair operations as gc. It does
// not restart failed guests; a higher-level scheduler owns restart policy.
func newDaemonCommand(configuration func() config.Config) *cobra.Command {
	reconcileInterval := 5 * time.Second
	var gcInterval time.Duration
	command := &cobra.Command{
		Use:   "daemon",
		Short: "supervise sandbox exits and optionally sweep orphaned resources",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			if reconcileInterval <= 0 || gcInterval < 0 {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("reconcile interval must be positive and GC interval must not be negative"))
			}
			resolved := configuration()
			if err := resolved.Validate(); err != nil {
				return err
			}
			service, err := core.OpenSnapshots(command.Context(), resolved, nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			lock := filelock.New(filepath.Join(resolved.Paths.Run, "locks", "daemon.lock"))
			acquired, err := lock.TryLock(command.Context())
			if err != nil {
				return err
			}
			if !acquired {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("another daemon owns this root"))
			}
			defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(command.Context()))) }()
			return supervise(command.Context(), service, reconcileInterval, gcInterval, command.ErrOrStderr())
		},
	}
	command.Flags().DurationVar(&reconcileInterval, "reconcile-interval", reconcileInterval, "interval for lifecycle state repair")
	command.Flags().DurationVar(&gcInterval, "gc-interval", 0, "interval for orphan collection (0 disables periodic GC)")
	return command
}

func supervise(ctx context.Context, service *core.SnapshotService, reconcileInterval, gcInterval time.Duration, diagnostics io.Writer) error {
	if reconcileInterval <= 0 || gcInterval < 0 {
		return errors.New("invalid supervisor intervals")
	}
	watcher, err := newExitWatcher()
	if err != nil {
		_, _ = fmt.Fprintf(diagnostics, "daemon: process notifications unavailable: %v\n", err)
	}
	var exitEvents <-chan struct{}
	if watcher != nil {
		exitEvents = watcher.Events()
		defer func() {
			if watcher != nil {
				_ = watcher.Close()
			}
		}()
	}
	syncWatcher := func() {
		if watcher == nil {
			return
		}
		statuses, err := service.Status(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(diagnostics, "daemon: observe processes: %v\n", err)
			return
		}
		if err := watcher.Sync(statuses); err != nil {
			_, _ = fmt.Fprintf(diagnostics, "daemon: process notifications disabled: %v\n", err)
			_ = watcher.Close()
			watcher = nil
			exitEvents = nil
		}
	}
	reconcile := func() {
		actions, skipped, err := service.ReconcileSandboxes(ctx)
		if err != nil || len(actions) > 0 {
			_, _ = fmt.Fprintf(diagnostics, "daemon: reconciled=%d skipped=%d error=%v\n", len(actions), skipped, err)
		}
		syncWatcher()
	}
	reconcile()
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()
	var gcTicker *time.Ticker
	var gcTicks <-chan time.Time
	if gcInterval > 0 {
		gcTicker = time.NewTicker(gcInterval)
		gcTicks = gcTicker.C
		defer gcTicker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reconcileTicker.C:
			reconcile()
		case <-exitEvents:
			reconcile()
		case <-gcTicks:
			report, err := service.Collect(ctx)
			if err != nil || len(report.Actions) > 0 {
				_, _ = fmt.Fprintf(diagnostics, "daemon: collected=%d skipped=%d error=%v\n", len(report.Actions), report.Skipped, err)
			}
			syncWatcher()
		}
	}
}
