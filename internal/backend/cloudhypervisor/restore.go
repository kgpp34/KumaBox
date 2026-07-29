package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// RestoreVM launches an API-only Cloud Hypervisor process, restores native
// state, and resumes vCPU execution. The source directory is private staging
// prepared by runtime and may therefore be patched in place.
func (b Backend) RestoreVM(ctx context.Context, rec *vmstore.VMRecord, sourceDir, mode string) (_ *backend.StartResult, err error) {
	return b.restoreNativeVM(ctx, rec, sourceDir, mode, nativeRestorePlan{})
}

// CloneVM restores a snapshot paused, replaces its source NIC devices with
// the clone's provider allocations, and only then resumes guest execution.
func (b Backend) CloneVM(ctx context.Context, rec *vmstore.VMRecord, sourceDir, mode string) (*backend.StartResult, error) {
	return b.restoreNativeVM(ctx, rec, sourceDir, mode, nativeRestorePlan{
		useCloneRestoreTaps: true,
		beforeResume: func(client *http.Client, config map[string]json.RawMessage) error {
			return hotSwapCloneNetworks(ctx, client, config, rec)
		},
	})
}

type nativeRestorePlan struct {
	useCloneRestoreTaps bool
	beforeResume        func(*http.Client, map[string]json.RawMessage) error
}

func (b Backend) restoreNativeVM(ctx context.Context, rec *vmstore.VMRecord, sourceDir, mode string, plan nativeRestorePlan) (_ *backend.StartResult, err error) {
	if rec == nil {
		return nil, errors.New("VM record is nil")
	}
	rendered, err := readRenderedConfig(rec.Config)
	if err != nil {
		return nil, fmt.Errorf("read backend launch config: %w", err)
	}
	nativeConfig, err := patchRestoreConfig(filepath.Join(sourceDir, snapshot.NativeConfigFile), rec, plan.useCloneRestoreTaps)
	if err != nil {
		return nil, fmt.Errorf("patch native restore config: %w", err)
	}
	if err := reapInterruptedRestore(*rendered); err != nil {
		return nil, err
	}
	cleanupRuntimeFiles(rec.RunDir)
	launch := *rendered
	launch.Args = []string{"--api-socket", rendered.APISocket}
	result, err := startProcess(launch)
	if err != nil {
		return nil, fmt.Errorf("launch Cloud Hypervisor restore process: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		_ = terminateProcess(result.PID, rendered.Binary, rendered.APISocket)
		cleanupRuntimeFiles(rec.RunDir)
	}()

	request, requestErr := nativeRestoreRequest(sourceDir, mode)
	if requestErr != nil {
		return nil, requestErr
	}
	if err = putJSONOnce(ctx, rendered.APISocket, nativeSnapshotTimeout, apiVMRestore, request, http.StatusNoContent); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	client := socketHTTPClient(rendered.APISocket, nativeSnapshotTimeout)
	defer client.CloseIdleConnections()
	if plan.beforeResume != nil {
		if err = plan.beforeResume(client, nativeConfig); err != nil {
			return nil, err
		}
	}
	if err = stateTransition(ctx, &vmstore.VMRecord{Config: rec.Config, APISocket: rendered.APISocket}, apiVMResume, backendStateRunning); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}
	return result, nil
}

type restoreRequest struct {
	SourceURL         string `json:"source_url"`
	MemoryRestoreMode string `json:"memory_restore_mode,omitempty"`
}

func nativeRestoreRequest(sourceDir, mode string) (restoreRequest, error) {
	request := restoreRequest{SourceURL: (&url.URL{Scheme: "file", Path: sourceDir}).String()}
	switch mode {
	case "copy":
	case "ondemand":
		request.MemoryRestoreMode = "OnDemand"
	case "mmap":
		request.MemoryRestoreMode = "Mmap"
	default:
		return restoreRequest{}, fmt.Errorf("RESTORE_MODE_UNSUPPORTED: %s", mode)
	}
	return request, nil
}

func reapInterruptedRestore(cfg Config) error {
	pid, err := readPIDFile(cfg.PIDFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read interrupted restore pid: %w", err)
	}
	if !processAlive(pid) {
		return nil
	}
	if err := terminateProcess(pid, cfg.Binary, cfg.APISocket); err != nil {
		return fmt.Errorf("terminate interrupted restore process: %w", err)
	}
	return nil
}

// patchRestoreConfig preserves backend-owned and future fields while replacing
// only host-local paths. Device order and identities were checked by preflight.
func patchRestoreConfig(path string, rec *vmstore.VMRecord, useCloneRestoreTaps bool) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	var disks []map[string]json.RawMessage
	if err := json.Unmarshal(config["disks"], &disks); err != nil {
		return nil, fmt.Errorf("decode disks: %w", err)
	}
	paths, err := restoreDiskPaths(rec, len(disks))
	if err != nil {
		return nil, err
	}
	if len(disks) != len(paths) {
		return nil, fmt.Errorf("disk count mismatch: native=%d target=%d", len(disks), len(paths))
	}
	for i := range disks {
		if err := setRawField(disks[i], "path", paths[i]); err != nil {
			return nil, err
		}
	}
	patchedDisks, err := json.Marshal(disks)
	if err != nil {
		return nil, fmt.Errorf("encode disks: %w", err)
	}
	config["disks"] = patchedDisks
	if useCloneRestoreTaps {
		if err := patchCloneRestoreTaps(config, rec); err != nil {
			return nil, err
		}
	}
	if serial, found := config["serial"]; found {
		var serialConfig map[string]json.RawMessage
		if err := json.Unmarshal(serial, &serialConfig); err != nil {
			return nil, fmt.Errorf("decode serial: %w", err)
		}
		var mode string
		if rawMode, ok := serialConfig["mode"]; ok {
			if err := json.Unmarshal(rawMode, &mode); err != nil {
				return nil, fmt.Errorf("decode serial mode: %w", err)
			}
		}
		if strings.EqualFold(mode, "file") {
			if err := patchRawPath(config, "serial", "file", filepath.Join(rec.LogDir, "console.log")); err != nil {
				return nil, err
			}
		}
	}
	if rec.VsockSocket != "" {
		if err := patchRawPath(config, "vsock", "socket", rec.VsockSocket); err != nil {
			return nil, err
		}
	}
	if err := fileutil.WriteJSONAtomic(path, config, ".restore-config-*.tmp"); err != nil {
		return nil, err
	}
	return config, nil
}

// patchCloneRestoreTaps replaces the snapshot TAPs with names that are unique
// to this restore. Cloud Hypervisor owns these transient TAPs until the guest
// ACKs device eject; only then can hotSwapCloneNetworks attach the clone's
// provider-owned CNI TAPs. Reusing provider TAPs here races with hot-add and
// can leave the restored guest's virtio and vsock devices unstable.
func patchCloneRestoreTaps(config map[string]json.RawMessage, rec *vmstore.VMRecord) error {
	raw, found := config["net"]
	if !found || string(raw) == "null" {
		if len(rec.NetworkConfigs) == 0 {
			return nil
		}
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot has 0 NICs, clone has %d", len(rec.NetworkConfigs))
	}
	var nets []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nets); err != nil {
		return fmt.Errorf("decode snapshot networks: %w", err)
	}
	if len(nets) != len(rec.NetworkConfigs) {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot has %d NICs, clone has %d", len(nets), len(rec.NetworkConfigs))
	}
	for i := range nets {
		if err := setRawField(nets[i], "tap", cloneRestoreTAPName(rec.ID, i)); err != nil {
			return err
		}
	}
	patched, err := json.Marshal(nets)
	if err != nil {
		return fmt.Errorf("encode snapshot networks: %w", err)
	}
	config["net"] = patched
	return nil
}

func cloneRestoreTAPName(vmID string, index int) string {
	const prefix = "rm"
	if len(vmID) > 8 {
		vmID = vmID[:8]
	}
	return fmt.Sprintf("%s%s-%d", prefix, vmID, index)
}

func hotSwapCloneNetworks(ctx context.Context, client *http.Client, config map[string]json.RawMessage, rec *vmstore.VMRecord) error {
	var oldNets []struct {
		ID string `json:"id"`
	}
	if raw := config["net"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &oldNets); err != nil {
			return fmt.Errorf("decode snapshot networks: %w", err)
		}
	}
	if len(oldNets) != len(rec.NetworkConfigs) {
		return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot has %d NICs, clone has %d", len(oldNets), len(rec.NetworkConfigs))
	}
	for i, oldNet := range oldNets {
		if oldNet.ID == "" {
			return fmt.Errorf("SNAPSHOT_INCOMPATIBLE: snapshot NIC %d has no backend device id", i)
		}
		body, err := json.Marshal(map[string]string{"id": oldNet.ID})
		if err != nil {
			return err
		}
		if _, err := doAPIOnceWithClient(ctx, client, http.MethodPut, apiVMRemoveDevice, body, http.StatusNoContent); err != nil {
			return fmt.Errorf("remove snapshot NIC %s: %w", oldNet.ID, err)
		}
	}
	for i, nc := range rec.NetworkConfigs {
		payload := map[string]any{
			"id":           cloneNetworkDeviceID(nc.MAC),
			"tap":          nc.TAP,
			"mac":          nc.MAC,
			"num_queues":   nc.NumQueues,
			"queue_size":   nc.QueueSize,
			"offload_tso":  true,
			"offload_ufo":  true,
			"offload_csum": true,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := doAPIOnceWithClient(ctx, client, http.MethodPut, apiVMAddNet, body, http.StatusOK, http.StatusNoContent); err != nil {
			return fmt.Errorf("add clone NIC %d: %w", i, err)
		}
	}
	return nil
}

func cloneNetworkDeviceID(mac string) string {
	return "kumabox-net-" + strings.ReplaceAll(strings.ToLower(mac), ":", "")
}

func restoreDiskPaths(rec *vmstore.VMRecord, nativeCount int) ([]string, error) {
	paths := make([]string, 0, len(rec.StorageConfigs)+1)
	for _, disk := range rec.StorageConfigs {
		paths = append(paths, disk.Path)
	}
	if nativeCount == len(paths)+1 && rec.Metadata != nil && rec.Metadata.CidataDisk != "" {
		paths = append(paths, rec.Metadata.CidataDisk)
	}
	if len(paths) != nativeCount {
		return nil, fmt.Errorf("native disk count %d cannot be mapped to target storage", nativeCount)
	}
	return paths, nil
}

func patchRawPath(config map[string]json.RawMessage, objectKey, fieldKey, value string) error {
	raw, ok := config[objectKey]
	if !ok || string(raw) == "null" {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("decode %s: %w", objectKey, err)
	}
	if err := setRawField(object, fieldKey, value); err != nil {
		return err
	}
	patched, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("encode %s: %w", objectKey, err)
	}
	config[objectKey] = patched
	return nil
}

func setRawField(object map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	object[key] = raw
	return nil
}
