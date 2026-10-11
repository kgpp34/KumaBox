//go:build linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/types"
)

// configureProcess places the guest command in its own process group so
// cancellation cannot leave background descendants behind.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

func configureIdentity(command *exec.Cmd, name string) (map[string]string, error) {
	if name == "" {
		return nil, nil
	}
	var account *user.User
	var err error
	if _, parseErr := strconv.ParseUint(name, 10, 32); parseErr == nil {
		account, err = user.LookupId(name)
	} else {
		account, err = user.Lookup(name)
	}
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	if strconv.Itoa(os.Geteuid()) != account.Uid || strconv.Itoa(os.Getegid()) != account.Gid {
		groups, groupErr := account.GroupIds()
		if groupErr != nil {
			return nil, groupErr
		}
		credentials := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		for _, group := range groups {
			id, parseErr := strconv.ParseUint(group, 10, 32)
			if parseErr != nil {
				return nil, parseErr
			}
			credentials.Groups = append(credentials.Groups, uint32(id))
		}
		command.SysProcAttr.Credential = credentials
	}
	return map[string]string{"HOME": account.HomeDir, "USER": account.Username, "LOGNAME": account.Username}, nil
}

// waitManagedProcess leaves the group leader as a zombie until the signal gate
// is held. Its PID/PGID cannot be recycled between a signal and the final reap.
// Remaining group members are killed before draining pipes and releasing the ID.
func waitManagedProcess(command *exec.Cmd, gate *sync.Mutex, exited *bool) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, command.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			gate.Lock()
			defer gate.Unlock()
			*exited = true
			_ = command.Process.Kill()
			return errors.Join(err, command.Wait())
		}
		break
	}
	gate.Lock()
	defer gate.Unlock()
	*exited = true
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	return command.Wait()
}

func signalManagedProcess(command *exec.Cmd, signal types.ProcessSignal) error {
	return syscall.Kill(-command.Process.Pid, processSignal(signal))
}

func processSignal(signal types.ProcessSignal) syscall.Signal {
	switch signal {
	case types.ProcessSignalTERM:
		return syscall.SIGTERM
	case types.ProcessSignalINT:
		return syscall.SIGINT
	case types.ProcessSignalHUP:
		return syscall.SIGHUP
	default:
		return syscall.SIGKILL
	}
}

// processExitCode maps signal termination to the conventional shell status
// 128+signal so the host CLI can preserve meaningful guest results.
func processExitCode(state *os.ProcessState) int {
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}
