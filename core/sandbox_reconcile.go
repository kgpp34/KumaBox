package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// ReconcileAction describes one completed repair of durable sandbox state.
type ReconcileAction struct {
	ID     types.SandboxID `json:"id"`
	Name   string          `json:"name"`
	Action string          `json:"action"`
}

// ReconcileSandboxes repairs interrupted lifecycle transitions one sandbox at
// a time. Busy operation locks are skipped; the next pass will retry them.
// No live Running VM is stopped as a side effect of reconciliation.
func (s *SandboxService) ReconcileSandboxes(ctx context.Context) (actions []ReconcileAction, skipped int, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return nil, 0, errors.New("sandbox reconciliation service is not configured")
	}
	records, err := s.dependencies.catalog.List(ctx)
	if err != nil {
		return nil, 0, err
	}
	var failures []error
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		action, busy, err := s.reconcileSandbox(ctx, record)
		if busy {
			skipped++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("sandbox %s: %w", record.ID, err))
			continue
		}
		if action != "" {
			actions = append(actions, ReconcileAction{ID: record.ID, Name: record.Config.Name, Action: action})
		}
	}
	return actions, skipped, errors.Join(failures...)
}

func (s *SandboxService) reconcileSandbox(ctx context.Context, observed types.Sandbox) (action string, busy bool, returnErr error) {
	path, err := s.dependencies.paths.Lock(observed.ID)
	if err != nil {
		return "", false, err
	}
	lock := filelock.New(path)
	acquired, err := lock.TryLock(ctx)
	if err != nil || !acquired {
		return "", !acquired, err
	}
	// Deleting is committed under the same lock that Create holds. The normal
	// Remove workflow can then resume it without racing an in-flight creator.
	remove := false
	defer func() {
		returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx)))
		if remove && returnErr == nil {
			_, returnErr = s.Remove(ctx, observed.ID.String())
		}
	}()
	record, err := s.dependencies.catalog.Resolve(ctx, observed.ID.String())
	if err != nil {
		return "", false, err
	}
	switch record.State {
	case types.SandboxStateCreating:
		if _, err := s.dependencies.catalog.BeginDelete(ctx, record.ID, record.Generation, s.dependencies.now().UTC()); err != nil {
			return "", false, err
		}
		remove = true
		return "removed-stale-create", false, nil
	case types.SandboxStateDeleting:
		remove = true
		return "finished-delete", false, nil
	case types.SandboxStateStarting, types.SandboxStateRunning, types.SandboxStateStopping:
		backend, err := s.dependencies.runtimes.Backend(record.VMM)
		if err != nil {
			return "", false, err
		}
		generation, err := stopProcessGeneration(record)
		if err != nil {
			return "", false, err
		}
		observation, err := backend.Observe(ctx, record.ID, generation)
		if err != nil {
			return "", false, err
		}
		if record.State == types.SandboxStateStarting && observation.State == vmm.ProcessRunning {
			_, err := s.dependencies.catalog.MarkRunning(ctx, record.ID, record.Generation, s.dependencies.now().UTC())
			return "recovered-running", false, err
		}
		if record.State != types.SandboxStateStopping && observation.State != vmm.ProcessAbsent {
			return "", false, nil
		}
		if record.State == types.SandboxStateStopping && observation.State != vmm.ProcessAbsent {
			if err := backend.Stop(ctx, observation.Process); err != nil {
				return "", false, err
			}
		}
		if err := backend.Cleanup(ctx, record.ID); err != nil {
			return "", false, err
		}
		if err := s.quiesceNetwork(ctx, record); err != nil {
			return "", false, err
		}
		if _, err := s.dependencies.catalog.MarkStopped(ctx, record.ID, record.Generation, record.State, s.dependencies.now().UTC()); err != nil {
			return "", false, err
		}
		return "recovered-stopped", false, nil
	case types.SandboxStateCreated, types.SandboxStateStopped, types.SandboxStateError:
		return "", false, nil
	default:
		return "", false, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("unknown sandbox state %q", record.State))
	}
}
