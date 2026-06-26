package runtime

import (
	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type Runtime struct {
	store    *vmstore.Store
	renderer backend.Renderer
}

func New(cfg config.Config) *Runtime {
	return NewWithRenderer(vmstore.New(cfg.Runtime.RootDir), cloudhypervisor.NewRenderer(cfg))
}

func NewWithRenderer(store *vmstore.Store, renderer backend.Renderer) *Runtime {
	return &Runtime{
		store:    store,
		renderer: renderer,
	}
}

func (r *Runtime) CreateVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.store.Create(req)
	if err != nil {
		return nil, err
	}
	if err := r.renderer.RenderConfig(rec); err != nil {
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	return rec, nil
}
