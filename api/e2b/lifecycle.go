package e2b

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

const pauseDescription = "kumabox:e2b-pause"

func sandboxID(w http.ResponseWriter, r *http.Request) (types.SandboxID, bool) {
	id, err := types.ParseSandboxID(r.PathValue("id"))
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return "", false
	}
	return id, true
}

// pauseSandbox assumes the API lease lock. Hibernate publishes a native memory
// snapshot and stops the VMM; the lease points at it only after that succeeds.
func (h *handler) pauseSandbox(ctx context.Context, id types.SandboxID, policy lease) error {
	record, err := h.sandboxes.Inspect(ctx, id.String())
	if err != nil {
		return err
	}
	if record.State != types.SandboxStateRunning {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is not running", id))
	}
	snapshot, err := h.snapshots.Hibernate(ctx, api.SaveSnapshotInput{
		SandboxReference: id.String(), Description: pauseDescription,
	})
	if err != nil {
		if snapshot.ID == "" {
			return err
		}
		// Hibernate may report a committed cleanup or progress error after
		// stopping the sandbox. Keep the snapshot discoverable for reconnect.
		current, inspectErr := h.sandboxes.Inspect(ctx, id.String())
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if current.State != types.SandboxStateStopped {
			return err
		}
	}
	policy.PausedSnapshot = snapshot.ID
	policy.RetainedSnapshots = append(policy.RetainedSnapshots, snapshot.ID)
	policy.ExpiresAt = time.Time{}
	if err := h.leases.put(id, policy); err != nil {
		return errdefs.WithContext(errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err), errdefs.ContextInfo{
			Operation: "pause E2B sandbox", Entity: id.String(), Phase: "persist pause lease",
			Action: fmt.Sprintf("inspect sandbox and snapshot %s before retrying", snapshot.ID), Committed: true,
		})
	}
	h.leases.schedule(id, time.Time{}, h.expire)
	return err
}

func (h *handler) pause(w http.ResponseWriter, r *http.Request) {
	id, ok := sandboxID(w, r)
	if !ok {
		return
	}
	if r.ContentLength != 0 {
		var input struct {
			Memory *bool `json:"memory"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		if input.Memory != nil && !*input.Memory {
			failure(w, http.StatusNotImplemented, "unimplemented", "filesystem-only pause is not supported")
			return
		}
	}
	unlock, err := h.leases.lock(r.Context(), id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	defer unlock()
	policy, exists, err := h.leases.get(id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	if !exists {
		failure(w, http.StatusNotFound, "not_found", "E2B sandbox lease not found")
		return
	}
	if policy.PausedSnapshot != "" {
		failure(w, http.StatusConflict, "state_conflict", "sandbox is already paused")
		return
	}
	if err := h.pauseSandbox(r.Context(), id, policy); err != nil {
		serviceFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) setTimeout(w http.ResponseWriter, r *http.Request) {
	id, ok := sandboxID(w, r)
	if !ok {
		return
	}
	var input struct {
		Timeout int `json:"timeout"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if input.Timeout <= 0 || input.Timeout > 86400 {
		failure(w, http.StatusBadRequest, "invalid_argument", "timeout must be between 1 and 86400 seconds")
		return
	}
	unlock, err := h.leases.lock(r.Context(), id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	defer unlock()
	policy, exists, err := h.leases.get(id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	if !exists {
		failure(w, http.StatusNotFound, "not_found", "E2B sandbox lease not found")
		return
	}
	if policy.PausedSnapshot != "" {
		failure(w, http.StatusConflict, "state_conflict", "resume sandbox before setting a timeout")
		return
	}
	policy.ExpiresAt = time.Now().UTC().Add(time.Duration(input.Timeout) * time.Second)
	if err := h.leases.put(id, policy); err != nil {
		serviceFailure(w, err)
		return
	}
	h.leases.schedule(id, policy.ExpiresAt, h.expire)
	w.WriteHeader(http.StatusNoContent)
}

// expire rereads the durable deadline after acquiring the sandbox lock. A
// renewed lease cannot be reaped by an older timer callback.
func (h *handler) expire(id types.SandboxID) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.expireLocked(ctx, id); err != nil {
		slog.Error("expire E2B sandbox", "sandbox", id, "error", err)
		h.leases.schedule(id, time.Now().Add(5*time.Second), h.expire)
	}
}

func (h *handler) expireLocked(ctx context.Context, id types.SandboxID) error {
	unlock, err := h.leases.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	policy, exists, err := h.leases.get(id)
	if err != nil || !exists || policy.ExpiresAt.IsZero() {
		return err
	}
	if time.Until(policy.ExpiresAt) > 0 {
		h.leases.schedule(id, policy.ExpiresAt, h.expire)
		return nil
	}
	if policy.AutoPause {
		record, err := h.sandboxes.Inspect(ctx, id.String())
		if err != nil {
			if isMissingSandbox(err) {
				h.removePauseSnapshots(ctx, policy)
				return h.leases.remove(id)
			}
			return err
		}
		if record.State == types.SandboxStateRunning {
			return h.pauseSandbox(ctx, id, policy)
		}
	}
	return h.killSandbox(ctx, id, policy)
}

func isMissingSandbox(err error) bool {
	var classified *errdefs.Error
	return errors.As(err, &classified) && classified.Class == errdefs.ClassNotFound
}

func (h *handler) killSandbox(ctx context.Context, id types.SandboxID, policy lease) error {
	record, err := h.sandboxes.Inspect(ctx, id.String())
	if err != nil {
		if isMissingSandbox(err) {
			h.removePauseSnapshots(ctx, policy)
			return h.leases.remove(id)
		}
		return err
	}
	if record.State == types.SandboxStateRunning {
		if _, err := h.sandboxes.Stop(ctx, id.String()); err != nil {
			return err
		}
	}
	if _, err := h.sandboxes.Remove(ctx, id.String()); err != nil {
		return err
	}
	h.removePauseSnapshots(ctx, policy)
	return h.leases.remove(id)
}

func (h *handler) removePauseSnapshots(ctx context.Context, policy lease) {
	if policy.PausedSnapshot != "" && len(policy.RetainedSnapshots) == 0 {
		policy.RetainedSnapshots = append(policy.RetainedSnapshots, policy.PausedSnapshot)
	}
	for _, snapshotID := range policy.RetainedSnapshots {
		if _, err := h.snapshots.Remove(ctx, snapshotID.String()); err != nil {
			slog.Warn("remove E2B pause snapshot", "snapshot", snapshotID, "error", err)
		}
	}
}

func (h *handler) connectSandbox(ctx context.Context, id types.SandboxID, timeout time.Duration) (types.Sandbox, error) {
	unlock, err := h.leases.lock(ctx, id)
	if err != nil {
		return types.Sandbox{}, err
	}
	defer unlock()
	policy, exists, err := h.leases.get(id)
	if err != nil {
		return types.Sandbox{}, err
	}
	if !exists {
		return types.Sandbox{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeArtifactUnavailable, errors.New("E2B sandbox lease not found"))
	}
	record, err := h.sandboxes.Inspect(ctx, id.String())
	if err != nil {
		return types.Sandbox{}, err
	}
	switch {
	case policy.PausedSnapshot != "" && record.State == types.SandboxStateStopped:
		record, err = h.snapshots.Restore(ctx, id.String(), policy.PausedSnapshot.String())
		if err != nil {
			return record, err
		}
		policy.PausedSnapshot = ""
	case record.State != types.SandboxStateRunning:
		return types.Sandbox{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not paused or running", id, record.State))
	default:
		// A prior Restore may have committed before its lease write failed.
		policy.PausedSnapshot = ""
	}
	deadline := time.Now().UTC().Add(timeout)
	if policy.ExpiresAt.Before(deadline) {
		policy.ExpiresAt = deadline
	}
	if err := h.leases.put(id, policy); err != nil {
		return record, err
	}
	h.leases.schedule(id, policy.ExpiresAt, h.expire)
	return record, nil
}

func (h *handler) resume(w http.ResponseWriter, r *http.Request) {
	h.connectWithStatus(w, r, http.StatusCreated)
}
