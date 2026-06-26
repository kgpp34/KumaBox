package backend

import "github.com/kumabox/kumabox/internal/vmstore"

type Renderer interface {
	RenderConfig(*vmstore.VMRecord) error
}

type StartResult struct {
	PID       int
	APISocket string
}

type Starter interface {
	StartConfig(string) (*StartResult, error)
}
