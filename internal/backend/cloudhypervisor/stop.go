package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	terminateGrace           = 2 * time.Second
	defaultStopTimeout       = 10 * time.Second
	backendAPIRequestTimeout = 2 * time.Second
	processPollInterval      = 100 * time.Millisecond
)

func (b Backend) StopVM(rec *vmstore.VMRecord, opts backend.StopOptions) (*backend.StopResult, error) {
	return b.stopper.StopVM(rec, opts)
}

type Stopper struct{}

func NewStopper() Stopper {
	return Stopper{}
}

func (Stopper) StopVM(rec *vmstore.VMRecord, opts backend.StopOptions) (*backend.StopResult, error) {
	if rec == nil {
		return nil, fmt.Errorf("VM record is nil")
	}
	cfg, err := readRenderedConfig(rec.Config)
	if err != nil {
		return nil, fmt.Errorf("read backend config: %w", err)
	}
	apiSocket := rec.APISocket
	if apiSocket == "" {
		apiSocket = cfg.APISocket
	}
	if rec.PID <= 0 {
		if rec.Restore != nil {
			pid, pidErr := readPIDFile(cfg.PIDFile)
			if pidErr == nil && processAlive(pid) {
				if err := terminateProcess(pid, cfg.Binary, apiSocket); err != nil {
					return nil, fmt.Errorf("stop interrupted restore process: %w", err)
				}
			} else if pidErr != nil && !errors.Is(pidErr, os.ErrNotExist) {
				return nil, fmt.Errorf("read interrupted restore pid: %w", pidErr)
			}
		}
		cleanupRuntimeFiles(rec.RunDir)
		return &backend.StopResult{}, nil
	}
	if !processAlive(rec.PID) {
		cleanupRuntimeFiles(rec.RunDir)
		return &backend.StopResult{}, nil
	}

	matched, reason := verifyProcessIdentity(rec.PID, cfg.Binary, apiSocket)
	if !matched {
		return nil, fmt.Errorf("refusing to stop VM: %s", reason)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	if !opts.Force {
		_ = resumeIfPaused(context.Background(), apiSocket)
		_ = shutdownVM(context.Background(), apiSocket)
		if waitForExit(rec.PID, timeout) {
			cleanupRuntimeFiles(rec.RunDir)
			return &backend.StopResult{}, nil
		}
	}

	if err := terminateProcess(rec.PID, cfg.Binary, apiSocket); err != nil {
		return nil, err
	}
	cleanupRuntimeFiles(rec.RunDir)
	return &backend.StopResult{}, nil
}

func shutdownVM(ctx context.Context, apiSocket string) error {
	_, err := doAPIOnce(ctx, apiSocket, backendAPIRequestTimeout, http.MethodPut, apiVMShutdown, nil, http.StatusNoContent)
	return err
}

func resumeIfPaused(ctx context.Context, apiSocket string) error {
	info, err := queryVMInfo(ctx, apiSocket, backendAPIRequestTimeout)
	if err != nil || !strings.EqualFold(info.State, backendStatePaused) {
		return err
	}
	_, err = doAPIOnce(ctx, apiSocket, backendAPIRequestTimeout, http.MethodPut, apiVMResume, nil, http.StatusNoContent)
	return err
}

func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(processPollInterval)
	}
}

func terminateProcess(pid int, binary string, apiSocket string) error {
	matched, reason := verifyProcessIdentity(pid, binary, apiSocket)
	if !matched {
		if !processAlive(pid) {
			return nil
		}
		return fmt.Errorf("refusing to terminate process: %s", reason)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && processAlive(pid) {
		_ = proc.Kill()
	}
	if waitForExit(pid, terminateGrace) {
		return nil
	}
	if err := proc.Kill(); err != nil && processAlive(pid) {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	if !waitForExit(pid, terminateGrace) {
		return fmt.Errorf("process %d did not exit after SIGKILL", pid)
	}
	return nil
}

func cleanupRuntimeFiles(runDir string) {
	for _, name := range []string{"ch.pid", "ch.sock"} {
		err := os.Remove(filepath.Join(runDir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
	}
}
