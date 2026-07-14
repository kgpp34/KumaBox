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

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// RestoreVM launches an API-only Cloud Hypervisor process, restores native
// state, and resumes vCPU execution. The source directory is private staging
// prepared by runtime and may therefore be patched in place.
func (b Backend) RestoreVM(ctx context.Context, rec *vmstore.VMRecord, sourceDir, mode string) (_ *backend.StartResult, err error) {
	if rec == nil {
		return nil, errors.New("VM record is nil")
	}
	if mode != "copy" {
		return nil, fmt.Errorf("RESTORE_MODE_UNSUPPORTED: %s", mode)
	}
	rendered, err := readRenderedConfig(rec.Config)
	if err != nil {
		return nil, fmt.Errorf("read backend launch config: %w", err)
	}
	if err := patchRestoreConfig(filepath.Join(sourceDir, "config.json"), rec); err != nil {
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

	sourceURL := (&url.URL{Scheme: "file", Path: sourceDir}).String()
	if err = putJSONOnce(ctx, rendered.APISocket, nativeSnapshotTimeout, "vm.restore", map[string]string{
		"source_url": sourceURL,
	}, http.StatusNoContent); err != nil {
		return nil, fmt.Errorf("vm.restore: %w", err)
	}
	if err = stateTransition(ctx, &vmstore.VMRecord{Config: rec.Config, APISocket: rendered.APISocket}, "vm.resume", "Running"); err != nil {
		return nil, fmt.Errorf("vm.resume: %w", err)
	}
	return result, nil
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
func patchRestoreConfig(path string, rec *vmstore.VMRecord) error {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	var disks []map[string]json.RawMessage
	if err := json.Unmarshal(config["disks"], &disks); err != nil {
		return fmt.Errorf("decode disks: %w", err)
	}
	paths, err := restoreDiskPaths(rec, len(disks))
	if err != nil {
		return err
	}
	if len(disks) != len(paths) {
		return fmt.Errorf("disk count mismatch: native=%d target=%d", len(disks), len(paths))
	}
	for i := range disks {
		if err := setRawField(disks[i], "path", paths[i]); err != nil {
			return err
		}
	}
	patchedDisks, err := json.Marshal(disks)
	if err != nil {
		return fmt.Errorf("encode disks: %w", err)
	}
	config["disks"] = patchedDisks
	if err := patchRawPath(config, "serial", "file", filepath.Join(rec.LogDir, "console.log")); err != nil {
		return err
	}
	if rec.VsockSocket != "" {
		if err := patchRawPath(config, "vsock", "socket", rec.VsockSocket); err != nil {
			return err
		}
	}
	return fileutil.WriteJSONAtomic(path, config, ".restore-config-*.tmp")
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
