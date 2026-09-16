//go:build linux

package cgroup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// cpuPeriodMicros is the standard 100 ms CFS bandwidth period.
const cpuPeriodMicros int64 = 100_000

// scopeDir derives a leaf only from a validated sandbox ID.
func (m *Manager) scopeDir(id types.SandboxID) (string, error) {
	if m == nil || m.parent == "" {
		return "", errors.New("cgroup manager is not configured")
	}
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return "", err
	}
	return filepath.Join(m.parent, "sandbox-"+id.String()+".scope"), nil
}

// writeControl writes one kernel cgroup control file with operation context.
func writeControl(directory, name, value string) error {
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return fmt.Errorf("write cgroup control %s: %w", path, err)
	}
	return nil
}

// Prepare creates or converges a CPU-controlled leaf and opens it for
// CLONE_INTO_CGROUP. Reusing an empty leaf makes interrupted starts retryable.
func (m *Manager) Prepare(_ context.Context, id types.SandboxID, cpus uint32) (*os.File, error) {
	if m == nil || m.parent == "" {
		return nil, errors.New("cgroup manager is not configured")
	}
	if cpus == 0 {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("cgroup vCPU count must be positive"))
	}
	if err := enableCPUHierarchy(m.parent); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	directory, err := m.scopeDir(id)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create cgroup scope: %w", err)
	}
	weight := min(int(cpus), 10_000)
	if err := writeControl(directory, "cpu.weight", strconv.Itoa(weight)); err != nil {
		return nil, err
	}
	quota := int64(cpus) * cpuPeriodMicros
	if err := writeControl(directory, "cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriodMicros)); err != nil {
		return nil, err
	}
	scope, err := os.Open(directory) //nolint:gosec // directory derives from fixed parent and validated UUID
	if err != nil {
		return nil, fmt.Errorf("open cgroup scope: %w", err)
	}
	return scope, nil
}

// PIDs returns every positive process currently owned by a sandbox scope.
func (m *Manager) PIDs(id types.SandboxID) ([]int, error) {
	directory, err := m.scopeDir(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(directory, "cgroup.procs")) //nolint:gosec // fixed file under validated scope
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open cgroup.procs: %w", err)
	}
	defer file.Close() //nolint:errcheck // scan error remains primary and read-only close cannot change ownership
	var result []int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("parse cgroup PID %q", scanner.Text())
		}
		result = append(result, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan cgroup.procs: %w", err)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

// Remove deletes an empty scope. Callers must prove the VMM absent first; this
// method never sends cgroup.kill because an unverified process must survive.
func (m *Manager) Remove(ctx context.Context, id types.SandboxID) error {
	directory, err := m.scopeDir(id)
	if err != nil {
		return err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := os.Remove(directory)
		switch {
		case err == nil || errors.Is(err, fs.ErrNotExist):
			return nil
		case !errors.Is(err, syscall.EBUSY) && !errors.Is(err, syscall.ENOTEMPTY):
			return fmt.Errorf("remove cgroup scope: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("remove busy cgroup scope %s: %w", directory, syscall.EBUSY)
		case <-ticker.C:
		}
	}
}

// enableCPUHierarchy enables delegation at root and at the KumaBox parent.
func enableCPUHierarchy(parent string) error {
	controllers, err := os.ReadFile(filepath.Join(Root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup v2 is unavailable: %w", err)
	}
	if !slices.Contains(strings.Fields(string(controllers)), "cpu") {
		return errors.New("cgroup v2 CPU controller is unavailable")
	}
	relative, err := filepath.Rel(Root, parent)
	if err != nil {
		return err
	}
	current := Root
	for element := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if err := enableCPU(current); err != nil {
			return err
		}
		current = filepath.Join(current, element)
		if err := os.Mkdir(current, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create cgroup parent %s: %w", current, err)
		}
	}
	return enableCPU(current)
}

// enableCPU avoids hierarchy-wide writes once the controller is active.
func enableCPU(directory string) error {
	path := filepath.Join(directory, "cgroup.subtree_control")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed control name under a validated cgroup hierarchy
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if slices.Contains(strings.Fields(string(raw)), "cpu") {
		return nil
	}
	return writeControl(directory, "cgroup.subtree_control", "+cpu")
}
