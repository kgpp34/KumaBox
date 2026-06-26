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
	starter  backend.Starter
}

func New(cfg config.Config) *Runtime {
	return NewWithBackend(vmstore.New(cfg.Runtime.RootDir), cloudhypervisor.NewRenderer(cfg), cloudhypervisor.NewStarter())
}

func NewWithBackend(store *vmstore.Store, renderer backend.Renderer, starter backend.Starter) *Runtime {
	return &Runtime{
		store:    store,
		renderer: renderer,
		starter:  starter,
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

func (r *Runtime) StartVM(ref string) (*vmstore.VMRecord, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}

	result, err := r.starter.StartConfig(rec.Config)
	if err != nil {
		if _, markErr := r.store.MarkError(rec.ID, err.Error()); markErr != nil {
			return nil, markErr
		}
		return nil, err
	}
	return r.store.MarkRunning(rec.ID, result.PID, result.APISocket)
}

func (r *Runtime) RunVM(req vmstore.CreateRequest) (*vmstore.VMRecord, error) {
	rec, err := r.CreateVM(req)
	if err != nil {
		return nil, err
	}
	started, err := r.StartVM(rec.ID)
	if err != nil {
		return nil, err
	}
	return started, nil
}
