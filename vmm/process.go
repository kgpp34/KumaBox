package vmm

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// ProcessOptions is host process policy, independent of a monitor's wire API.
type ProcessOptions struct {
	// Binary is an executable name or absolute path.
	Binary string
	// StartupTimeout bounds process/API readiness.
	StartupTimeout time.Duration
	// StopGrace bounds TERM before escalating to KILL.
	StopGrace time.Duration
	// AbortGrace bounds termination of a failed launch.
	AbortGrace time.Duration
}

// Resolve derives bounded defaults without looking up the monitor executable.
func (o ProcessOptions) Resolve(defaultBinary string) (ProcessOptions, error) {
	if o.Binary == "" {
		o.Binary = defaultBinary
	}
	if o.StartupTimeout == 0 {
		o.StartupTimeout = 10 * time.Second
	}
	if o.StopGrace == 0 {
		o.StopGrace = 5 * time.Second
	}
	if o.AbortGrace == 0 {
		o.AbortGrace = 3 * time.Second
	}
	if o.Binary == "" || o.StartupTimeout < 5*time.Millisecond || o.StopGrace <= 0 || o.AbortGrace <= 0 {
		return ProcessOptions{}, errors.New("VMM executable and bounded positive lifecycle timeouts are required")
	}
	return o, nil
}

// ProcessSource restricts crash recovery to the sandbox-owned cgroup.
type ProcessSource interface {
	PIDs(types.SandboxID) ([]int, error)
}

// LocateProcess validates a durable identity or recovers the exec-before-persist
// window from its cgroup. It never searches unrelated host processes.
func LocateProcess(paths Paths, source ProcessSource, id types.SandboxID, generation uint64, binary string) (Process, bool, error) {
	process, err := paths.ReadProcess(id)
	if errors.Is(err, fs.ErrNotExist) {
		process, err = recoverOwnedProcess(paths, source, id, generation, binary)
	}
	if err != nil {
		return Process{}, false, err
	}
	if process.PID == 0 {
		return Process{}, false, nil
	}
	alive, err := VerifyProcess(process)
	if err != nil {
		if errors.Is(err, ErrProcessIdentityPending) {
			// Keep the captured identity for readiness polling, but do not
			// expose it as a verified live process to other callers.
			if process.Generation != generation {
				return Process{}, false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("live VMM belongs to Starting generation %d, expected %d", process.Generation, generation))
			}
			return process, false, err
		}
		return Process{}, false, err
	}
	if !alive {
		return Process{}, false, nil
	}
	if process.Generation != generation {
		return Process{}, false, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("live VMM belongs to Starting generation %d, expected %d", process.Generation, generation))
	}
	return process, true, nil
}

// recoverProcess inspects only the sandbox's cgroup and refuses unknown members.
func recoverOwnedProcess(paths Paths, source ProcessSource, id types.SandboxID, generation uint64, binary string) (Process, error) {
	pids, err := source.PIDs(id)
	if err != nil {
		return Process{}, err
	}
	if len(pids) == 0 {
		return Process{}, nil
	}
	apiSocket, err := paths.APISocket(id)
	if err != nil {
		return Process{}, err
	}
	var matches []Process
	for _, pid := range pids {
		process, match, err := IdentifyProcess(pid, id, generation, filepath.Base(binary), apiSocket)
		if err != nil {
			return Process{}, err
		}
		if match {
			matches = append(matches, process)
		}
	}
	if len(matches) != 1 || len(pids) != 1 {
		return Process{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("cgroup contains %d process(es), %d matching the owned VMM", len(pids), len(matches)))
	}
	if err := paths.WriteProcess(matches[0]); err != nil {
		return Process{}, err
	}
	return matches[0], nil
}
