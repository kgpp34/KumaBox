package backend

import (
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

type Lifecycle interface {
	RenderConfig(*vmstore.VMRecord) error
	StartVM(*vmstore.VMRecord) (*StartResult, error)
	StopVM(*vmstore.VMRecord, StopOptions) (*StopResult, error)
	ObserveVM(*vmstore.VMRecord) vmstore.Observation
}

type StartResult struct {
	PID       int
	APISocket string
}

type StopOptions struct {
	Timeout time.Duration
	Force   bool
}

type StopResult struct{}
