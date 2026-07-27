package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const pciSysfsPrefix = "/sys/bus/pci/devices/"

func (b Backend) AttachPCIDevice(ctx context.Context, rec *vmstore.VMRecord, spec backend.PCIDeviceSpec) (backend.AttachedPCIDevice, error) {
	if rec == nil {
		return backend.AttachedPCIDevice{}, fmt.Errorf("VM record is nil")
	}
	path, err := normalizePCIPath(spec.PCI)
	if err != nil {
		return backend.AttachedPCIDevice{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return backend.AttachedPCIDevice{}, fmt.Errorf("stat PCI device: %w", err)
	}
	driver, err := os.Readlink(filepath.Join(path, "driver"))
	if err != nil {
		return backend.AttachedPCIDevice{}, fmt.Errorf("read PCI driver: %w", err)
	}
	if filepath.Base(driver) != "vfio-pci" {
		return backend.AttachedPCIDevice{}, fmt.Errorf("PCI device %s is bound to %s, want vfio-pci", path, filepath.Base(driver))
	}
	id := spec.ID
	if id == "" {
		id = "kumabox-pci-" + strings.ReplaceAll(strings.TrimPrefix(path, pciSysfsPrefix), ":", "-")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return backend.AttachedPCIDevice{}, err
	}
	for _, device := range info.Config.Devices {
		if device.ID == id || device.Path == path {
			return backend.AttachedPCIDevice{}, fmt.Errorf("PCI device %s is already attached", path)
		}
	}
	body, _ := json.Marshal(map[string]string{"id": id, "path": path})
	if _, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMAddDevice, body, http.StatusOK, http.StatusNoContent); err != nil {
		return backend.AttachedPCIDevice{}, err
	}
	return backend.AttachedPCIDevice{ID: id, PCI: path}, nil
}

func (b Backend) DetachPCIDevice(ctx context.Context, rec *vmstore.VMRecord, id string) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return err
	}
	for _, device := range info.Config.Devices {
		if device.ID == id {
			body, _ := json.Marshal(map[string]string{"id": id})
			_, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMRemoveDevice, body, http.StatusNoContent)
			return err
		}
	}
	return fmt.Errorf("PCI device %q is not attached", id)
}

func (b Backend) ListPCIDevices(ctx context.Context, rec *vmstore.VMRecord) ([]backend.AttachedPCIDevice, error) {
	if rec == nil {
		return nil, fmt.Errorf("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return nil, err
	}
	result := make([]backend.AttachedPCIDevice, 0)
	for _, device := range info.Config.Devices {
		if strings.HasPrefix(device.ID, "kumabox-pci-") {
			result = append(result, backend.AttachedPCIDevice{ID: device.ID, PCI: device.Path})
		}
	}
	return result, nil
}

func normalizePCIPath(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, pciSysfsPrefix) {
		value = strings.TrimPrefix(filepath.Clean(value), pciSysfsPrefix)
	} else if len(value) == 8 && value[4] == ':' {
	} else if len(value) == 7 && value[2] == ':' {
		value = "0000:" + value
	} else {
		return "", fmt.Errorf("PCI address %q is invalid", value)
	}
	return pciSysfsPrefix + value, nil
}
