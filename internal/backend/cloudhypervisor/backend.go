package cloudhypervisor

import (
	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

var _ backend.Lifecycle = Backend{}
var _ backend.StateController = Backend{}
var _ backend.NativeSnapshotter = Backend{}
var _ backend.NativeHostInspector = Backend{}
var _ backend.NativeRestorer = Backend{}
var _ backend.NativeCloner = Backend{}
var _ backend.DiskController = Backend{}
var _ backend.FilesystemController = Backend{}
var _ backend.PCIDeviceController = Backend{}
var _ backend.ConsoleController = Backend{}

type Backend struct {
	renderer Renderer
	starter  Starter
	stopper  Stopper
}

func NewBackend(cfg config.Config) Backend {
	return Backend{
		renderer: NewRenderer(cfg),
		starter:  NewStarter(),
		stopper:  NewStopper(),
	}
}

func (b Backend) RenderConfig(rec *vmstore.VMRecord) error {
	return b.renderer.RenderConfig(rec)
}

func (b Backend) StartVM(rec *vmstore.VMRecord) (*backend.StartResult, error) {
	return b.starter.StartConfig(rec.Config)
}

func (b Backend) ObserveVM(rec *vmstore.VMRecord) vmstore.Observation {
	return ObserveVM(rec)
}
