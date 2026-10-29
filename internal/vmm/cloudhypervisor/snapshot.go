package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

const (
	snapshotTimeout      = 10 * time.Minute
	balloonPollInterval  = 100 * time.Millisecond
	balloonStallTimeout  = 2 * time.Second
	balloonSettleTimeout = time.Minute
)

var (
	_ vmm.Snapshotter = (*Driver)(nil)
	_ vmm.Hibernator  = (*Driver)(nil)
)

// Snapshot pauses the exact owned process, captures native VMM state and every
// writable disk, then resumes the guest even when capture fails.
//
//	verify -> settle balloon -> pause -> native state -> writable disks -> resume
//	                             \----------- any error -----------/
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
	return d.Stop(ctx, plan.Process, types.StopOptions{Force: true})
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
	info, err := d.liveInfo(ctx, plan.Process)
	if err != nil {
		return err
	}
	if err := refuseExternalDevices(info); err != nil {
		return err
	}
	if err := d.waitBalloonSettled(ctx, plan.Process, info); err != nil {
		return err
	}
	if err := d.snapshotAction(ctx, plan.Process.APISocket, "vm.pause", nil, probeTimeout); err != nil {
		return fmt.Errorf("pause cloud-hypervisor: %w", err)
	}
	return nil
}

// waitBalloonSettled lets guest reclaim finish before pausing it. A stalled
// guest still has a valid snapshot: after two seconds without progress (or a
// minute overall), capture the actual RAM rather than blocking indefinitely.
func (d *Driver) waitBalloonSettled(ctx context.Context, process vmm.Process, info vmInfo) error {
	if info.Config.Balloon == nil || info.MemoryActualSize == 0 {
		return nil
	}
	target := info.Config.Memory.Size - info.Config.Balloon.Size
	started, progress := time.Now(), time.Now()
	ticker := time.NewTicker(balloonPollInterval)
	defer ticker.Stop()
	for actual := info.MemoryActualSize; actual > target; {
		if time.Since(progress) >= balloonStallTimeout || time.Since(started) >= balloonSettleTimeout {
			slog.WarnContext(ctx, "balloon reclaim has not reached its target; capturing current memory", "sandbox", process.SandboxID, "actual", actual, "target", target)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		current, err := d.liveInfo(ctx, process)
		if err != nil {
			return err
		}
		if current.MemoryActualSize < actual {
			progress = time.Now()
		}
		actual = current.MemoryActualSize
	}
	return nil
}

// refuseExternalDevices keeps captures independent of host-owned resources
// that are neither copied into the snapshot nor guaranteed after restart.
func refuseExternalDevices(info vmInfo) error {
	if len(info.Config.FS) > 0 || len(info.Config.Devices) > 0 {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("detach runtime file shares and PCI devices before snapshot or hibernate"))
	}
	if disks := runtimeDisks(info); len(disks) > 0 {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("detach external disk %q before snapshot or hibernate", disks[0].Name))
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
		if err := storage.CloneFile(file.Destination, file.Source); err != nil {
			return fmt.Errorf("capture writable disk: %w", err)
		}
	}
	return nil
}

func (d *Driver) snapshotAction(ctx context.Context, socket, endpoint string, payload []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	release, err := d.acquireAPI(ctx, socket)
	if err != nil {
		return err
	}
	defer release()
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
