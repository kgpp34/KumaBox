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
func (m *Manager) Prepare(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy) (*os.File, error) {
	return m.prepare(ctx, id, cpus, policy, false)
}

// PrepareRestore leaves CPU bandwidth unlimited while a paused VM loads its
// snapshot. The caller must ApplyLimit before resuming any guest execution.
func (m *Manager) PrepareRestore(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy) (*os.File, error) {
	return m.prepare(ctx, id, cpus, policy, true)
}

func (m *Manager) prepare(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy, restoring bool) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.parent == "" {
		return nil, errors.New("cgroup manager is not configured")
	}
	if _, err := policy.Resolve(cpus); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := enableCPUHierarchy(m.parent, policy.CPUSet != ""); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	directory, err := m.scopeDir(id)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create cgroup scope: %w", err)
	}
	if err := placeCPUs(m.parent, directory, policy.CPUSet); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	if err := writeCPULimits(directory, cpus, policy, restoring); err != nil {
		return nil, err
	}
	scope, err := os.Open(directory) //nolint:gosec // directory derives from fixed parent and validated UUID
	if err != nil {
		return nil, fmt.Errorf("open cgroup scope: %w", err)
	}
	return scope, nil
}

// ApplyLimit restores the runtime CPU ceiling after provisioning. A failed
// write must prevent vm.resume; the paused VMM is then reclaimed by its owner.
func (m *Manager) ApplyLimit(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cpus == 0 {
		return errors.New("cgroup vCPU count must be positive")
	}
	directory, err := m.scopeDir(id)
	if err != nil {
		return err
	}
	return writeCPULimits(directory, cpus, policy, false)
}

// writeCPULimits converges retryable scope controls before process placement.
func writeCPULimits(directory string, cpus uint32, policy types.CPUPolicy, restoring bool) error {
	resolved, err := policy.Resolve(cpus)
	if err != nil {
		return err
	}
	// Reset old burst credit before lowering quota: the kernel rejects a quota
	// smaller than existing credit. Missing burst support degrades only defaults.
	_, burstErr := os.Stat(filepath.Join(directory, "cpu.max.burst"))
	if burstErr != nil && (!errors.Is(burstErr, fs.ErrNotExist) || policy.BurstUS > 0) {
		return fmt.Errorf("cpu-burst-us requires cpu.max.burst support: %w", burstErr)
	}
	if burstErr == nil {
		if err := writeControl(directory, "cpu.max.burst", "0"); err != nil {
			return err
		}
	}
	if err := writeControl(directory, "cpu.weight", strconv.FormatUint(uint64(resolved.Weight), 10)); err != nil {
		return err
	}
	maximum := fmt.Sprintf("%d %d", resolved.QuotaUS, resolved.PeriodUS)
	if restoring {
		maximum = fmt.Sprintf("max %d", resolved.PeriodUS)
	}
	if err := writeControl(directory, "cpu.max", maximum); err != nil {
		return err
	}
	if burstErr == nil && !restoring {
		return writeControl(directory, "cpu.max.burst", strconv.FormatInt(max(0, resolved.BurstUS), 10))
	}
	return nil
}

// placeCPUs rejects kernel intersections rather than silently granting only
// part of a requested set. Clearing a reused scope restores inherited placement.
func placeCPUs(parent, directory, requested string) error {
	if requested == "" {
		if _, err := os.Stat(filepath.Join(directory, "cpuset.cpus")); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		// A zero-length write is a no-op on cgroupfs; a newline clears the set.
		return writeControl(directory, "cpuset.cpus", "\n")
	}
	available, err := os.ReadFile(filepath.Join(parent, "cpuset.cpus.effective")) //nolint:gosec // configured cgroup parent
	if err != nil {
		return fmt.Errorf("read available cpuset: %w", err)
	}
	contains, err := types.CPUSetContains(strings.TrimSpace(string(available)), requested)
	if err != nil || !contains {
		return fmt.Errorf("requested cpuset %q is outside effective CPUs %q: %w", requested, strings.TrimSpace(string(available)), errors.Join(err, errors.New("unavailable CPU placement")))
	}
	if err := writeControl(directory, "cpuset.cpus", requested); err != nil {
		return err
	}
	effective, err := os.ReadFile(filepath.Join(directory, "cpuset.cpus.effective")) //nolint:gosec // managed scope
	if err != nil {
		return err
	}
	actual := strings.TrimSpace(string(effective))
	forward, err := types.CPUSetContains(actual, requested)
	if err != nil || !forward {
		return errors.Join(err, fmt.Errorf("cpuset request %q was narrowed to %q", requested, actual))
	}
	backward, err := types.CPUSetContains(requested, actual)
	if err != nil || !backward {
		return errors.Join(err, fmt.Errorf("cpuset request %q differs from effective %q", requested, actual))
	}
	return nil
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
func enableCPUHierarchy(parent string, placement bool) error {
	controllers, err := os.ReadFile(filepath.Join(Root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup v2 is unavailable: %w", err)
	}
	if !slices.Contains(strings.Fields(string(controllers)), "cpu") {
		return errors.New("cgroup v2 CPU controller is unavailable")
	}
	wanted := []string{"cpu"}
	if placement {
		if !slices.Contains(strings.Fields(string(controllers)), "cpuset") {
			return errors.New("cgroup v2 cpuset controller is unavailable")
		}
		wanted = append(wanted, "cpuset")
	}
	relative, err := filepath.Rel(Root, parent)
	if err != nil {
		return err
	}
	current := Root
	for element := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if err := enableCPU(current, wanted); err != nil {
			return err
		}
		current = filepath.Join(current, element)
		if err := os.Mkdir(current, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create cgroup parent %s: %w", current, err)
		}
	}
	return enableCPU(current, wanted)
}

// enableCPU avoids hierarchy-wide writes once the controller is active.
func enableCPU(directory string, wanted []string) error {
	path := filepath.Join(directory, "cgroup.subtree_control")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed control name under a validated cgroup hierarchy
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var missing []string
	for _, controller := range wanted {
		if !slices.Contains(strings.Fields(string(raw)), controller) {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return writeControl(directory, "cgroup.subtree_control", strings.Join(missing, " "))
}
