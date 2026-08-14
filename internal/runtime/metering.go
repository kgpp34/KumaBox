package runtime

import (
	"context"
	"time"

	"github.com/kumabox/kumabox/internal/metering"
	"github.com/kumabox/kumabox/internal/vm"
)

func (r *Runtime) recordComputeStart(ctx context.Context, rec *vm.VMRecord, reason metering.Reason) {
	if r.storeSet.Metering == nil || rec == nil || rec.StartedAt == nil {
		return
	}
	_ = r.storeSet.Metering.Append(ctx, computeEvent(rec, metering.KindComputeStart, reason, *rec.StartedAt))
}

func (r *Runtime) recordComputeStop(ctx context.Context, rec *vm.VMRecord, reason metering.Reason) {
	if r.storeSet.Metering == nil || rec == nil || rec.StoppedAt == nil {
		return
	}
	_ = r.storeSet.Metering.Append(ctx, computeEvent(rec, metering.KindComputeStop, reason, *rec.StoppedAt))
}

func (r *Runtime) requireComputeStop(ctx context.Context, rec *vm.VMRecord, reason metering.Reason) error {
	if r.storeSet.Metering == nil || rec == nil || rec.StoppedAt == nil {
		return nil
	}
	return r.storeSet.Metering.Append(ctx, computeEvent(rec, metering.KindComputeStop, reason, *rec.StoppedAt))
}

func computeEvent(rec *vm.VMRecord, kind metering.Kind, reason metering.Reason, at time.Time) metering.Event {
	return metering.Event{
		ID: metering.EventID(rec.ID, kind, at), Kind: kind, VMID: rec.ID, VMName: rec.Name,
		Reason: reason, Shape: metering.Shape{VCPUs: rec.CPUs, MemoryBytes: rec.MemoryBytes}, EmittedAt: at,
	}
}

// ReconcileMetering idempotently reconstructs lifecycle endpoints represented
// by durable VM timestamps. It does not guess timestamps from wall-clock time.
func (r *Runtime) ReconcileMetering(ctx context.Context) error {
	if r.storeSet.Metering == nil {
		return nil
	}
	records, err := r.vmReader.List()
	if err != nil {
		return err
	}
	events, err := r.storeSet.Metering.Events(ctx, "")
	if err != nil {
		return err
	}
	existing := make(map[string]struct{}, len(events))
	for _, event := range events {
		existing[event.ID] = struct{}{}
	}
	for _, rec := range records {
		if rec.StartedAt != nil {
			reason := metering.ReasonBoot
			if rec.FirstBooted {
				reason = metering.ReasonRestart
			}
			event := computeEvent(rec, metering.KindComputeStart, reason, *rec.StartedAt)
			if _, ok := existing[event.ID]; !ok {
				if err := r.storeSet.Metering.Append(ctx, event); err != nil {
					return err
				}
			}
		}
		if rec.StoppedAt != nil {
			reason := metering.ReasonStopUser
			if rec.State == vm.StatePaused {
				reason = metering.ReasonPause
			}
			event := computeEvent(rec, metering.KindComputeStop, reason, *rec.StoppedAt)
			if _, ok := existing[event.ID]; ok {
				continue
			}
			if err := r.storeSet.Metering.Append(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}
