package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/vmm"
)

const (
	reseedRetryInterval = 25 * time.Millisecond
	reseedTimeout       = 8 * time.Second
)

// Reseed refreshes a running guest's random pool through its host-only agent
// channel. Machine ID renewal is intended for clones, not ordinary restore.
func (s *SandboxService) Reseed(ctx context.Context, reference string, machineID bool) error {
	backend, process, err := s.locateRunning(ctx, reference, "reseed sandbox")
	if err != nil {
		return err
	}
	if err := reseedProcess(ctx, backend, process, machineID); err != nil {
		return errdefs.WithContext(err, errdefs.ContextInfo{
			Operation: "reseed sandbox",
			Entity:    reference,
			Phase:     "contact guest agent",
			Action:    "inspect the guest agent service and retry",
		})
	}
	return nil
}

// reseedProcess retries only connection failures while a resumed guest agent
// reopens its vsock listener. A guest rejection is returned immediately.
func reseedProcess(ctx context.Context, backend vmm.Backend, process vmm.Process, machineID bool) error {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate host entropy: %w", err)
	}
	retryCtx, cancel := context.WithTimeout(ctx, reseedTimeout)
	defer cancel()
	var dialErr error
	for {
		connection, err := backend.DialVsock(retryCtx, process, agent.Port)
		if err == nil {
			err = agent.Reseed(retryCtx, connection, entropy, machineID)
			closeErr := connection.Close()
			return errors.Join(err, closeErr)
		}
		dialErr = err
		select {
		case <-retryCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("connect guest agent: %w", errors.Join(dialErr, retryCtx.Err()))
		case <-time.After(reseedRetryInterval):
		}
	}
}
