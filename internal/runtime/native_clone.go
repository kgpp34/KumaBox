package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/metering"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/resourceguard"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	cloneIdentityTimeout        = 15 * time.Second
	cloneIdentityAttemptTimeout = 5 * time.Second
	cloneIdentityRetryInterval  = 500 * time.Millisecond
)

var configureGuestIdentity = configureCloneIdentity

func guestAgentWarning(err error) string {
	if err == nil {
		return ""
	}
	return "VM is running, but guest post-restore configuration was incomplete: " + err.Error()
}

// NativeCloneOptions defines the new VM identity. Machine and storage shape
// are inherited from the native snapshot and cannot be resized during clone.
type NativeCloneOptions struct {
	Name     string
	Networks []string
	Mode     RestoreMode
}

// CloneNativeSnapshot creates a new running VM from native state while
// assigning fresh host storage, vsock, and provider network identities.
func (r *Runtime) CloneNativeSnapshot(ctx context.Context, snapshotRef string, opts NativeCloneOptions) (result *vmstore.VMRecord, resultErr error) {
	mutation, err := r.resourceGuard.BeginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer mutation.Release() //nolint:errcheck

	if opts.Name == "" {
		return nil, errors.New("clone VM name must not be empty")
	}
	operationID, err := r.beginOperationWithRelated(ctx, operation.KindSnapshotCloneNative, snapshotRef, snapshotRef)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = r.finishOperation(ctx, operationID, resultErr) }()
	mode, err := normalizeRestoreMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	opts.Mode = mode
	restoreStarted := time.Now()
	cloner, ok := r.backend.(backend.NativeCloner)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not support native clone")
	}
	inspector, ok := r.backend.(backend.NativeHostInspector)
	if !ok {
		return nil, errors.New("BACKEND_OPERATION_UNSUPPORTED: backend does not expose native compatibility")
	}

	snapshotStore := r.storeSet.Snapshots
	snapshotRec, lease, err := snapshotStore.AcquireRead(ctx, snapshotRef)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck
	host, err := inspector.InspectNativeHost(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect native compatibility: %w", err)
	}
	if err := requireRestoreMode(host, opts.Mode); err != nil {
		return nil, err
	}
	manifest, err := snapshotStore.VerifyNativePayloadRecord(ctx, snapshotRec, host)
	if err != nil {
		return nil, fmt.Errorf("snapshot preflight: %w", err)
	}
	networks, err := cloneNetworkSelections(opts.Networks, manifest.Devices.NICs)
	if err != nil {
		return nil, err
	}
	image, err := r.storeSet.Images.Inspect(manifest.Source.ImageID)
	if err != nil {
		return nil, fmt.Errorf("BASE_IMAGE_MISSING: resolve image %s: %w", manifest.Source.ImageID, err)
	}
	imageLock, err := r.resourceGuard.LockEntity(ctx, resourceguard.EntityImage, image.ID)
	if err != nil {
		return nil, err
	}
	defer imageLock.Release() //nolint:errcheck
	image, err = r.storeSet.Images.Inspect(image.ID)
	if err != nil {
		return nil, fmt.Errorf("BASE_IMAGE_MISSING: revalidate image %s: %w", manifest.Source.ImageID, err)
	}
	req, err := restoreCreateRequest(RestoreOptions{
		Name: opts.Name, CPUs: manifest.Machine.VCPUs, MemoryBytes: manifest.Machine.MemoryBytes, Networks: networks,
	}, image, manifest, r.cfg)
	if err != nil {
		return nil, err
	}
	rec, err := r.vmRecords.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.bindOperationResource(ctx, operationID, rec.ID); err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("bind clone operation resource: %w", err)
	}
	if err := r.recordVMImageReference(ctx, rec); err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("record clone image reference: %w", err)
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		_ = r.vmRecords.Delete(rec.ID)
		return nil, fmt.Errorf("lock clone VM %s: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck

	var backendResult *backend.StartResult
	committed := false
	defer func() {
		if committed {
			return
		}
		if backendResult != nil {
			cleanup := *rec
			cleanup.PID = backendResult.PID
			cleanup.APISocket = backendResult.APISocket
			_, _ = r.backend.StopVM(&cleanup, backend.StopOptions{Force: true})
		}
		// A native restore can fail after its destructive disk boundary. Keep
		// the VM and its provider attachments in an explicit error state so an
		// operator can inspect and delete it deliberately. Removing the record
		// here made clone failures indistinguishable from successful cleanup.
		if resultErr != nil {
			if _, markErr := r.vmRestore.FailRestore(rec.ID, resultErr.Error()); markErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("preserve failed clone state: %w", markErr))
			}
			return
		}
		r.network.rollbackNetwork(rec)
		_ = r.storage.removeManagedDirs(rec)
		_ = r.vmRecords.Delete(rec.ID)
	}()

	if err := r.network.attachNetwork(rec); err != nil {
		return nil, err
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	if err := snapshot.VerifyNativeCloneTarget(ctx, manifest, rec); err != nil {
		return nil, err
	}
	if err := r.backend.RenderConfig(rec); err != nil {
		return nil, fmt.Errorf("render clone launch config: %w", err)
	}
	staged, stageMetrics, err := stageNativeRestore(ctx, snapshotRec, manifest, rec)
	if err != nil {
		return nil, err
	}
	defer staged.cleanup() //nolint:errcheck
	dirty, err := r.vmRestore.BeginRestore(rec.ID, snapshotRec.ID, string(opts.Mode))
	if err != nil {
		return nil, err
	}
	diskCommitStarted := time.Now()
	if err := staged.commitDisks(); err != nil {
		return nil, fmt.Errorf("replace clone writable disks: %w", err)
	}
	diskCommitDuration := time.Since(diskCommitStarted)
	backendRestoreStarted := time.Now()
	backendResult, err = cloner.CloneVM(ctx, dirty, staged.nativeDir, string(opts.Mode))
	if err != nil {
		return nil, fmt.Errorf("restore clone backend state: %w", err)
	}
	backendRestoreDuration := time.Since(backendRestoreStarted)
	identityStarted := time.Now()
	// Identity configuration is a best-effort guest capability. Do not tear
	// down a successfully restored VMM when the image has no compatible agent.
	identityErr := configureGuestIdentity(ctx, rec.VsockSocket, rec)
	identityDuration := time.Since(identityStarted)
	cloned, err := r.vmRestore.CompleteRestore(rec.ID, backendResult.PID, backendResult.APISocket, time.Since(restoreStarted), &vmstore.RestoreResult{
		NativeStageDurationMs:    stageMetrics.nativeStageDuration.Milliseconds(),
		DiskStageDurationMs:      stageMetrics.diskStageDuration.Milliseconds(),
		DiskCommitDurationMs:     diskCommitDuration.Milliseconds(),
		BackendRestoreDurationMs: backendRestoreDuration.Milliseconds(),
		IdentityDurationMs:       identityDuration.Milliseconds(),
		GuestAgentWarning:        guestAgentWarning(identityErr),
	})
	if err != nil {
		return nil, err
	}
	r.recordComputeStart(ctx, cloned, metering.ReasonClone)
	if err := r.recordVMSnapshotReference(ctx, cloned.ID, snapshotRec.ID); err != nil {
		return nil, fmt.Errorf("record clone snapshot reference: %w", err)
	}
	if restoreModePinsSnapshot(opts.Mode) {
		staged.retainNativePayload()
	}
	committed = true
	_ = writeVMEvent(cloned, "snapshot.clone.completed", vmstore.Observation{
		State: vmstore.ObservedStateRunning, Reason: "cloned from native snapshot " + snapshotRec.ID, CheckedAt: time.Now().UTC(),
	})
	return r.applyObservation(cloned), nil
}

func cloneNetworkSelections(requested []string, nicCount int) ([]string, error) {
	if nicCount == 0 {
		if len(requested) > 0 && (len(requested) != 1 || requested[0] != "none") {
			return nil, errors.New("SNAPSHOT_INCOMPATIBLE: networkless snapshot cannot gain NICs during clone")
		}
		return []string{"none"}, nil
	}
	if len(requested) == 0 {
		requested = make([]string, nicCount)
		for i := range requested {
			requested[i] = "default"
		}
	}
	if len(requested) != nicCount {
		return nil, fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot has %d NICs, clone requested %d", nicCount, len(requested))
	}
	for _, network := range requested {
		if network == "none" {
			return nil, errors.New("SNAPSHOT_INCOMPATIBLE: none cannot be mixed with native clone NICs")
		}
	}
	return append([]string(nil), requested...), nil
}

func configureCloneIdentity(ctx context.Context, socket string, rec *vmstore.VMRecord) error {
	request := agentclient.IdentityRequest{Hostname: rec.Name, Interfaces: make([]agentclient.InterfaceIdentity, 0, len(rec.NetworkConfigs))}
	for i, config := range rec.NetworkConfigs {
		identity := agentclient.InterfaceIdentity{Name: config.IfName, MAC: config.MAC}
		if identity.Name == "" {
			identity.Name = kbnetwork.GuestInterfaceName(i)
		}
		if config.Network != nil {
			identity.IP = config.Network.IP
			identity.Prefix = config.Network.Prefix
			identity.Gateway = config.Network.Gateway
			identity.DNS = append([]string(nil), config.Network.DNS...)
		}
		request.Interfaces = append(request.Interfaces, identity)
	}
	identityCtx, cancel := context.WithTimeout(ctx, cloneIdentityTimeout)
	defer cancel()
	pong, err := agentclient.Ping(identityCtx, socket)
	if err != nil {
		return fmt.Errorf("wait for clone guest agent: %w", err)
	}
	if err := requireAgentCapability(pong, agentclient.CapabilityIdentity); err != nil {
		return err
	}
	var lastErr error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(identityCtx, cloneIdentityAttemptTimeout)
		_, err := agentclient.ConfigureIdentity(attemptCtx, socket, request)
		attemptCancel()
		if err == nil {
			break
		}
		lastErr = err
		if identityCtx.Err() != nil {
			return fmt.Errorf("configure clone guest identity: last attempt: %v: %w", lastErr, identityCtx.Err())
		}

		retry := time.NewTimer(cloneIdentityRetryInterval)
		select {
		case <-identityCtx.Done():
			if !retry.Stop() {
				select {
				case <-retry.C:
				default:
				}
			}
			return fmt.Errorf("configure clone guest identity: last attempt: %v: %w", lastErr, identityCtx.Err())
		case <-retry.C:
		}
	}
	if err := requireAgentCapability(pong, agentclient.CapabilityReseed); err != nil {
		return err
	}
	reseedCtx, reseedCancel := context.WithTimeout(identityCtx, cloneIdentityAttemptTimeout)
	defer reseedCancel()
	if _, err := agentclient.Reseed(reseedCtx, socket, true); err != nil {
		return fmt.Errorf("reseed clone guest identity: %w", err)
	}
	return nil
}
