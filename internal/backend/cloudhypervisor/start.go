package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
)

const (
	defaultAPISocketWaitTimeout = 5 * time.Second
	apiSocketPollInterval       = 50 * time.Millisecond
)

type Starter struct{}

func NewStarter() Starter {
	return Starter{}
}

func (Starter) StartConfig(path string) (*backend.StartResult, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read Cloud Hypervisor config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse Cloud Hypervisor config: %w", err)
	}
	return startProcess(cfg)
}

func startProcess(cfg Config) (*backend.StartResult, error) {
	if err := validateStartConfig(cfg); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.PIDFile), 0o755); err != nil {
		return nil, fmt.Errorf("create pid directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StdoutLog), 0o755); err != nil {
		return nil, fmt.Errorf("create stdout log directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StderrLog), 0o755); err != nil {
		return nil, fmt.Errorf("create stderr log directory: %w", err)
	}

	stdout, err := os.OpenFile(cfg.StdoutLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open stdout log: %w", err)
	}
	defer stdout.Close() //nolint:errcheck

	stderr, err := os.OpenFile(cfg.StderrLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open stderr log: %w", err)
	}
	defer stderr.Close() //nolint:errcheck

	cmd := exec.Command(cfg.Binary, cfg.Args...) //nolint:gosec
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := startInNetNS(cmd, cfg.NetnsPath); err != nil {
		return nil, fmt.Errorf("start Cloud Hypervisor: %w", err)
	}

	pid := cmd.Process.Pid
	if err := writePIDFile(cfg.PIDFile, pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()

	timeout := time.Duration(cfg.APITimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultAPISocketWaitTimeout
	}
	if err := waitForUnixSocket(cfg.APISocket, exited, timeout); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(cfg.PIDFile)
		return nil, err
	}

	return &backend.StartResult{PID: pid, APISocket: cfg.APISocket}, nil
}

func validateStartConfig(cfg Config) error {
	if cfg.Binary == "" {
		return fmt.Errorf("cloud hypervisor binary is empty")
	}
	if cfg.APISocket == "" {
		return fmt.Errorf("cloud hypervisor API socket is empty")
	}
	if cfg.PIDFile == "" {
		return fmt.Errorf("cloud hypervisor pid file is empty")
	}
	if cfg.StdoutLog == "" {
		return fmt.Errorf("cloud hypervisor stdout log is empty")
	}
	if cfg.StderrLog == "" {
		return fmt.Errorf("cloud hypervisor stderr log is empty")
	}
	return nil
}

func writePIDFile(path string, pid int) error {
	data := []byte(fmt.Sprintf("%d\n", pid))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}

func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid file %s", path)
	}
	return pid, nil
}

func waitForUnixSocket(path string, exited <-chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-exited:
			if err == nil {
				return fmt.Errorf("Cloud Hypervisor exited before API socket became ready")
			}
			return fmt.Errorf("Cloud Hypervisor exited before API socket became ready: %w", err)
		default:
		}
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Cloud Hypervisor API socket %s", path)
		}
		time.Sleep(apiSocketPollInterval)
	}
}
