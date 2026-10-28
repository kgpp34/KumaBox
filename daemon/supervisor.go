package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kumabox/kumabox/core"
)

// exitWatcher emits a hint when an identity-checked VMM process exits. The
// periodic reconciliation ticker remains the correctness floor.
type exitWatcher interface {
	Sync([]core.SandboxStatus) error
	Events() <-chan struct{}
	Close() error
}

// Supervise reconciles process exits and periodically collects orphan resources.
func Supervise(ctx context.Context, service *core.MaintenanceService, reconcileInterval, gcInterval time.Duration, diagnostics io.Writer) error {
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
