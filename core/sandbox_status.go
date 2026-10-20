package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// SandboxStatus combines a durable sandbox record with an identity-checked
// runtime observation. Observation never changes the stored lifecycle state.
type SandboxStatus struct {
	Sandbox types.Sandbox
	Runtime vmm.ProcessState
	PID     int
	Stale   bool
}

// Status observes every sandbox or the requested references. An absent VMM for
// a Running record is reported as stale so callers can distinguish a crash
// from a clean Stop without mutating metadata during a read.
func (s *SandboxService) Status(ctx context.Context, references ...string) ([]SandboxStatus, error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return nil, errors.New("sandbox status service is not configured")
	}
	var records []types.Sandbox
	if len(references) == 0 {
		var err error
		records, err = s.dependencies.catalog.List(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		records = make([]types.Sandbox, 0, len(references))
		seen := make(map[types.SandboxID]bool, len(references))
		for _, reference := range references {
			record, err := s.dependencies.catalog.Resolve(ctx, reference)
			if err != nil {
				return nil, err
			}
			if !seen[record.ID] {
				records = append(records, record)
				seen[record.ID] = true
			}
		}
	}
	statuses := make([]SandboxStatus, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		status, err := s.observeStatus(ctx, record)
		if err != nil {
			return nil, fmt.Errorf("observe sandbox %s: %w", record.ID, err)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *SandboxService) observeStatus(ctx context.Context, record types.Sandbox) (SandboxStatus, error) {
	status := SandboxStatus{Sandbox: record}
	var generation uint64
	switch record.State {
	case types.SandboxStateStarting:
		generation = record.Generation
	case types.SandboxStateRunning:
		if record.Generation < 2 {
			return status, errors.New("running sandbox has no Starting generation")
		}
		generation = record.Generation - 1
	case types.SandboxStateStopping:
		if record.Generation < 3 {
			return status, errors.New("stopping sandbox has no Starting generation")
		}
		generation = record.Generation - 2
	default:
		return status, nil
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return status, err
	}
	observation, err := backend.Observe(ctx, record.ID, generation)
	if err != nil {
		return status, err
	}
	status.Runtime = observation.State
	if observation.State != vmm.ProcessAbsent {
		status.PID = observation.Process.PID
	}
	status.Stale = record.State == types.SandboxStateRunning && observation.State == vmm.ProcessAbsent
	return status, nil
}
