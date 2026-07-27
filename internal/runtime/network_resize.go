package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/backend"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func (r *Runtime) ResizeNetwork(ctx context.Context, ref string, target int) (*vmstore.VMRecord, error) {
	if target < 0 {
		return nil, fmt.Errorf("network count must not be negative")
	}
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return nil, err
	}
	lock, err := r.vmLocks.Acquire(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("lock VM %s for network resize: %w", rec.ID, err)
	}
	defer lock.Release() //nolint:errcheck
	controller, ok := r.backend.(backend.NetworkController)
	if !ok {
		return nil, fmt.Errorf("backend does not support network resize")
	}
	rec, err = r.vmReader.Inspect(rec.ID)
	if err != nil {
		return nil, err
	}
	if rec.State != vmstore.StateRunning {
		return nil, fmt.Errorf("VM must be running")
	}
	opID, err := r.beginOperation(ctx, operation.KindNetworkResize, rec.ID)
	if err != nil {
		return nil, err
	}
	opErr := r.resizeNetworksLocked(ctx, controller, rec, target)
	opErr = r.finishOperation(ctx, opID, opErr)
	updated, inspectErr := r.vmReader.Inspect(rec.ID)
	return updated, errors.Join(opErr, inspectErr)
}

func (r *Runtime) resizeNetworksLocked(ctx context.Context, controller backend.NetworkController, rec *vmstore.VMRecord, target int) error {
	current := len(rec.NetworkConfigs)
	if target == current {
		return nil
	}
	if target > current {
		selection := "default"
		if len(rec.Networks) > 0 {
			selection = rec.Networks[0]
		}
		added := make([]kbnetwork.Config, 0, target-current)
		for index := current; index < target; index++ {
			allocation, err := r.network.attachNetworkConfig(rec, selection, index)
			if err != nil {
				for _, previous := range added {
					_ = controller.DetachNetwork(ctx, rec, previous)
				}
				rollbackNetworkConfigs(rec, r.cfg, added)
				return err
			}
			if err := controller.AttachNetwork(ctx, rec, allocation.Config); err != nil {
				for _, previous := range added {
					_ = controller.DetachNetwork(ctx, rec, previous)
				}
				rollbackNetworkConfigs(rec, r.cfg, append(added, allocation.Config))
				return err
			}
			added = append(added, allocation.Config)
		}
		_, err := r.vmRecords.SetNetworkConfigs(rec.ID, append(append([]kbnetwork.Config(nil), rec.NetworkConfigs...), added...))
		return err
	}
	for index := current - 1; index >= target; index-- {
		network := rec.NetworkConfigs[index]
		if err := controller.DetachNetwork(ctx, rec, network); err != nil {
			return err
		}
		if err := cleanupNetworkConfig(ctx, r.storeSet.Networks, kbnetwork.NewAllocator(r.cfg.Runtime.RootDir, r.cfg.Network), r.cfg, rec, network, false); err != nil {
			return err
		}
	}
	_, err := r.vmRecords.SetNetworkConfigs(rec.ID, append([]kbnetwork.Config(nil), rec.NetworkConfigs[:target]...))
	return err
}
