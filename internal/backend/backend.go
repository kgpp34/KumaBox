package backend

import "github.com/kumabox/kumabox/internal/vmstore"

type Lifecycle interface {
	RenderConfig(*vmstore.VMRecord) error
	StartVM(*vmstore.VMRecord) (*StartResult, error)
}

type StartResult struct {
	PID       int
	APISocket string
}
