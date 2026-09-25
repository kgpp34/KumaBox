package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/vmm"
)

const snapshotTimeout = 10 * time.Minute

var (
	_ vmm.Snapshotter = (*Driver)(nil)
	_ vmm.Hibernator  = (*Driver)(nil)
)

// Snapshot pauses the exact owned process, captures native VMM state and every
// writable disk, then resumes the guest even when capture fails.
//
//	verify -> pause -> native state -> writable disks -> resume
//	             \----------- any error -----------/
func (d *Driver) Snapshot(ctx context.Context, plan vmm.SnapshotPlan) (returnErr error) {
	if err := d.pauseForCapture(ctx, plan); err != nil {
		return err
	}
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.startupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, d.snapshotAction(resumeCtx, plan.Process.APISocket, "vm.resume", nil, d.startupTimeout))
	}()
	return d.capturePaused(ctx, plan)
}

// Hibernate keeps the VM paused until persist has made its capture durable.
// Once termination starts, ownership stays with the caller's Stopping record.
//
//	pause -> capture -> persist -> stop
//	           \--- failure: resume ---/
func (d *Driver) Hibernate(ctx context.Context, plan vmm.SnapshotPlan, persist func() error) (returnErr error) {
	if persist == nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("hibernate requires a persistence callback"))
	}
	if err := d.pauseForCapture(ctx, plan); err != nil {
		return err
	}
	shouldResume := true
	defer func() {
		if !shouldResume {
			return
		}
		resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.startupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, d.snapshotAction(resumeCtx, plan.Process.APISocket, "vm.resume", nil, d.startupTimeout))
	}()
	if err := d.capturePaused(ctx, plan); err != nil {
		return err
	}
	if err := persist(); err != nil {
		return err
	}
	shouldResume = false
	return d.Stop(ctx, plan.Process)
}

func (d *Driver) pauseForCapture(ctx context.Context, plan vmm.SnapshotPlan) error {
	if err := plan.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	observation, err := d.Observe(ctx, plan.Process.SandboxID, plan.Process.Generation)
	if err != nil {
		return err
	}
	if observation.State != vmm.ProcessRunning || observation.Process.PID != plan.Process.PID || observation.Process.StartTicks != plan.Process.StartTicks {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox VMM changed before snapshot capture"))
	}
	if err := d.snapshotAction(ctx, plan.Process.APISocket, "vm.pause", nil, probeTimeout); err != nil {
		return fmt.Errorf("pause cloud-hypervisor: %w", err)
	}
	return nil
}

func (d *Driver) capturePaused(ctx context.Context, plan vmm.SnapshotPlan) error {
	payload, err := json.Marshal(map[string]string{"destination_url": "file://" + plan.Destination})
	if err != nil {
		return err
	}
	if err := d.snapshotAction(ctx, plan.Process.APISocket, "vm.snapshot", payload, snapshotTimeout); err != nil {
		return fmt.Errorf("capture cloud-hypervisor state: %w", err)
	}
	for _, file := range plan.WritableFiles {
		if err := storage.CopySparse(file.Destination, file.Source); err != nil {
			return fmt.Errorf("capture writable disk: %w", err)
		}
	}
	return nil
}

func (d *Driver) snapshotAction(ctx context.Context, socket, endpoint string, payload []byte, timeout time.Duration) error {
	client, closeClient, err := unixAPIClient(socket)
	if err != nil {
		return err
	}
	defer closeClient()
	client.Timeout = timeout
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // status and bounded body are authoritative
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxAPIResponse))
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cloud hypervisor %s returned HTTP %d: %s", endpoint, response.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
