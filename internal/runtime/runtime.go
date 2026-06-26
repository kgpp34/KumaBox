package runtime

import (
	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type Runtime struct {
	cfg   config.Config
	store *vmstore.Store
}

func New(cfg config.Config) *Runtime {
	return &Runtime{
		cfg:   cfg,
		store: vmstore.New(cfg.Runtime.RootDir),
	}
}

func (r *Runtime) CreateVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.store.Create(req)
	if err != nil {
		return nil, err
	}
	if err := cloudhypervisor.RenderConfig(r.cfg, rec); err != nil {
		_ = r.store.Delete(rec.ID)
		return nil, err
	}
	return rec, nil
}
