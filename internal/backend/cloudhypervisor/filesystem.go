package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vm"
)

const filesystemIDPrefix = "kumabox-fs-"

func (b Backend) AttachFilesystem(ctx context.Context, rec *vm.VMRecord, spec backend.FilesystemSpec) (backend.AttachedFilesystem, error) {
	if rec == nil {
		return backend.AttachedFilesystem{}, fmt.Errorf("VM record is nil")
	}
	if !rec.SharedMemory {
		return backend.AttachedFilesystem{}, fmt.Errorf("virtio-fs requires shared memory at VM creation")
	}
	if spec.Socket == "" || spec.Tag == "" {
		return backend.AttachedFilesystem{}, fmt.Errorf("socket and tag are required")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return backend.AttachedFilesystem{}, err
	}
	id := filesystemIDPrefix + spec.Tag
	for _, fs := range info.Config.Fs {
		if fs.ID == id || fs.Tag == spec.Tag {
			return backend.AttachedFilesystem{}, fmt.Errorf("filesystem tag %q is already attached", spec.Tag)
		}
	}
	body, err := json.Marshal(map[string]any{"id": id, "tag": spec.Tag, "socket": spec.Socket, "num_queues": spec.NumQueues, "queue_size": spec.QueueSize})
	if err != nil {
		return backend.AttachedFilesystem{}, err
	}
	if _, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMAddFS, body, http.StatusOK, http.StatusNoContent); err != nil {
		return backend.AttachedFilesystem{}, err
	}
	return backend.AttachedFilesystem{ID: id, Tag: spec.Tag, Socket: spec.Socket}, nil
}

func (b Backend) DetachFilesystem(ctx context.Context, rec *vm.VMRecord, tag string) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return err
	}
	for _, fs := range info.Config.Fs {
		if fs.Tag == tag || fs.ID == filesystemIDPrefix+tag {
			body, _ := json.Marshal(map[string]string{"id": fs.ID})
			_, err := doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMRemoveDevice, body, http.StatusNoContent)
			return err
		}
	}
	return fmt.Errorf("filesystem tag %q is not attached", tag)
}

func (b Backend) ListFilesystems(ctx context.Context, rec *vm.VMRecord) ([]backend.AttachedFilesystem, error) {
	if rec == nil {
		return nil, fmt.Errorf("VM record is nil")
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendAPIRequestTimeout)
	if err != nil {
		return nil, err
	}
	result := make([]backend.AttachedFilesystem, 0, len(info.Config.Fs))
	for _, fs := range info.Config.Fs {
		if strings.HasPrefix(fs.ID, filesystemIDPrefix) {
			result = append(result, backend.AttachedFilesystem{ID: fs.ID, Tag: fs.Tag, Socket: fs.Socket})
		}
	}
	return result, nil
}
