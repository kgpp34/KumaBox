package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

var _ vmm.BalloonResizer = (*Driver)(nil)

// ResizeBalloon updates only a running owned VM and leaves pause ownership intact.
func (d *Driver) ResizeBalloon(
	ctx context.Context,
	process vmm.Process,
	target types.BalloonTarget,
) (types.BalloonStatus, error) {
	info, err := d.liveInfo(ctx, process)
	if err != nil {
		return types.BalloonStatus{}, err
	}
	status, err := balloonStatus(info)
	if err != nil {
		return types.BalloonStatus{}, err
	}
	if status == nil {
		return types.BalloonStatus{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("sandbox has no balloon device"))
	}
	size, err := target.Resolve(info.Config.Memory.Size)
	if err != nil {
		return types.BalloonStatus{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := d.resizeBalloon(ctx, process.APISocket, size); err != nil {
		return types.BalloonStatus{}, err
	}
	status.Target = size
	return *status, nil
}

func (d *Driver) resizeBalloon(ctx context.Context, socket string, size int64) error {
	payload, err := json.Marshal(struct {
		Size int64 `json:"desired_balloon"`
	}{Size: size})
	if err != nil {
		return err
	}
	return d.snapshotAction(ctx, socket, "vm.resize", payload, d.startupTimeout)
}

// balloonStatus projects an existing info response without another API request.
func balloonStatus(info vmInfo) (*types.BalloonStatus, error) {
	if info.Config.Balloon == nil {
		return nil, nil
	}
	memory, target := info.Config.Memory.Size, info.Config.Balloon.Size
	invalidTarget := target < 0 || target >= memory
	invalidActual := info.MemoryActualSize < 0 || info.MemoryActualSize > memory
	if memory <= 0 || invalidTarget || invalidActual {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("VMM reported invalid balloon memory sizes"))
	}
	status := &types.BalloonStatus{Target: target, Default: types.DefaultBalloonSize(memory)}
	if info.MemoryActualSize > 0 {
		actual := memory - info.MemoryActualSize
		status.Actual = &actual
	}
	return status, nil
}

// cloneBalloonTarget restores the create-time target after loading native state.
// Ordinary restore keeps the captured target. A settled default needs no API call.
func cloneBalloonTarget(config map[string]json.RawMessage) (*int64, error) {
	var info vmInfo
	if raw := config["balloon"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &info.Config.Balloon); err != nil {
			return nil, fmt.Errorf("decode captured balloon: %w", err)
		}
	}
	if info.Config.Balloon == nil {
		return nil, nil
	}
	if err := json.Unmarshal(config["memory"], &info.Config.Memory); err != nil {
		return nil, fmt.Errorf("decode captured memory: %w", err)
	}
	status, err := balloonStatus(info)
	if err != nil {
		return nil, err
	}
	if status.Target == status.Default {
		return nil, nil
	}
	return &status.Default, nil
}
