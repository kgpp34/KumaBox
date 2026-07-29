//go:build !linux

package server

import (
	"context"
	"os/exec"
)

func monitorProcess(ctx context.Context, cmd *exec.Cmd) chan struct{} {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
		case <-done:
		}
	}()
	return done
}

func configureProcess(_ *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
