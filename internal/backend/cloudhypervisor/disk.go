package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vm"
)

const (
	diskIDPrefix       = "kumabox-disk-"
	cloudHypervisorRaw = "Raw"
)

func (b Backend) AttachDisk(ctx context.Context, rec *vm.VMRecord, spec backend.DiskSpec) (backend.AttachedDisk, error) {
	if rec == nil {
		return backend.AttachedDisk{}, errors.New("VM record is nil")
	}
	if !filepath.IsAbs(spec.Path) {
		return backend.AttachedDisk{}, fmt.Errorf("disk path must be absolute")
	}
	if !validDiskName(spec.Name) {
		return backend.AttachedDisk{}, fmt.Errorf("disk name %q is invalid", spec.Name)
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return backend.AttachedDisk{}, err
	}
	if !strings.EqualFold(info.State, backendStateRunning) {
		return backend.AttachedDisk{}, fmt.Errorf("VM must be running")
	}
	st, err := os.Stat(spec.Path)
	if err != nil {
		return backend.AttachedDisk{}, fmt.Errorf("stat disk: %w", err)
	}
	if !st.Mode().IsRegular() {
		return backend.AttachedDisk{}, fmt.Errorf("disk path is not a regular file")
	}
	id := diskIDPrefix + spec.Name
	for _, disk := range info.Config.Disks {
		if disk.ID == id || disk.Serial == spec.Name || disk.Path == spec.Path {
			return backend.AttachedDisk{}, fmt.Errorf("disk %q is already attached", spec.Name)
		}
	}
	direct := false
	if spec.DirectIO != nil {
		direct = *spec.DirectIO
	}
	body := map[string]any{"id": id, "path": spec.Path, "readonly": spec.ReadOnly, "direct": direct, "image_type": cloudHypervisorRaw, "serial": spec.Name}
	if _, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMAddDisk, mustJSON(body), http.StatusOK, http.StatusNoContent); err != nil {
		return backend.AttachedDisk{}, err
	}
	return backend.AttachedDisk{ID: id, Name: spec.Name, Path: spec.Path, ReadOnly: spec.ReadOnly}, nil
}

func (b Backend) DetachDisk(ctx context.Context, rec *vm.VMRecord, name string) error {
	if rec == nil {
		return errors.New("VM record is nil")
	}
	if !validDiskName(name) {
		return fmt.Errorf("disk name %q is invalid", name)
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return err
	}
	id := diskIDPrefix + name
	for _, disk := range info.Config.Disks {
		if disk.ID == id || disk.Serial == name {
			_, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMRemoveDevice, mustJSON(map[string]string{"id": disk.ID}), http.StatusNoContent)
			return err
		}
	}
	return fmt.Errorf("disk %q is not attached", name)
}

func (b Backend) ListDisks(ctx context.Context, rec *vm.VMRecord) ([]backend.AttachedDisk, error) {
	if rec == nil {
		return nil, errors.New("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return nil, err
	}
	result := make([]backend.AttachedDisk, 0)
	for _, disk := range info.Config.Disks {
		if !strings.HasPrefix(disk.ID, diskIDPrefix) {
			continue
		}
		name := strings.TrimPrefix(disk.ID, diskIDPrefix)
		if validDiskName(name) {
			result = append(result, backend.AttachedDisk{ID: disk.ID, Name: name, Path: disk.Path, ReadOnly: disk.ReadOnly})
		}
	}
	return result, nil
}

func validDiskName(name string) bool {
	if len(name) == 0 || len(name) > 20 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name[1:] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func mustJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }
