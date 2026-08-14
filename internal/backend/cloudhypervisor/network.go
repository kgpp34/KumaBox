package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kumabox/kumabox/internal/backend"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vm"
)

var _ backend.NetworkController = Backend{}

func (b Backend) AttachNetwork(ctx context.Context, rec *vm.VMRecord, network kbnetwork.Config) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	body, err := json.Marshal(map[string]any{"id": network.ID, "tap": network.TAP, "mac": network.MAC, "num_queues": network.NumQueues, "queue_size": network.QueueSize, "offload_tso": true, "offload_ufo": true, "offload_csum": true})
	if err != nil {
		return err
	}
	_, err = doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMAddNet, body, http.StatusOK, http.StatusNoContent)
	return err
}

func (b Backend) DetachNetwork(ctx context.Context, rec *vm.VMRecord, network kbnetwork.Config) error {
	if rec == nil {
		return fmt.Errorf("VM record is nil")
	}
	body, err := json.Marshal(map[string]string{"id": network.ID})
	if err != nil {
		return err
	}
	_, err = doAPIOnce(ctx, rec.APISocket, backendAPIRequestTimeout, http.MethodPut, apiVMRemoveDevice, body, http.StatusNoContent)
	return err
}
