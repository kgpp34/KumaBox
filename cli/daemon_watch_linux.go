//go:build linux

package cli

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/types"
)

type watchedProcess struct {
	pid        int
	generation uint64
	fd         int
}

// pidfdWatcher uses one epoll descriptor for all VMM exits. Readable pidfds
// are one-shot hints; a full reconciliation verifies identity before mutation.
type pidfdWatcher struct {
	epfd    int
	watched map[types.SandboxID]watchedProcess
	events  chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func newExitWatcher() (exitWatcher, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	watcher := &pidfdWatcher{
		epfd: epfd, watched: make(map[types.SandboxID]watchedProcess),
		events: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go watcher.wait()
	return watcher, nil
}

func (w *pidfdWatcher) Sync(statuses []core.SandboxStatus) error {
	wanted := make(map[types.SandboxID]core.SandboxStatus, len(statuses))
	for _, status := range statuses {
		if status.PID > 0 {
			wanted[status.Sandbox.ID] = status
		}
	}
	for id, current := range w.watched {
		status, keep := wanted[id]
		if keep && status.PID == current.pid && status.Sandbox.Generation == current.generation {
			continue
		}
		_ = unix.EpollCtl(w.epfd, unix.EPOLL_CTL_DEL, current.fd, nil)
		_ = unix.Close(current.fd)
		delete(w.watched, id)
	}
	for id, status := range wanted {
		if _, exists := w.watched[id]; exists {
			continue
		}
		fd, err := unix.PidfdOpen(status.PID, 0)
		if errors.Is(err, syscall.ESRCH) {
			select {
			case w.events <- struct{}{}:
			default:
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("open pidfd for sandbox %s: %w", id, err)
		}
		event := &unix.EpollEvent{Events: unix.EPOLLIN | unix.EPOLLONESHOT, Fd: int32(fd)} //nolint:gosec // process descriptors fit epoll's int32 field
		if err := unix.EpollCtl(w.epfd, unix.EPOLL_CTL_ADD, fd, event); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("watch sandbox %s: %w", id, err)
		}
		w.watched[id] = watchedProcess{pid: status.PID, generation: status.Sandbox.Generation, fd: fd}
	}
	return nil
}

func (w *pidfdWatcher) Events() <-chan struct{} { return w.events }

func (w *pidfdWatcher) wait() {
	defer close(w.done)
	events := make([]unix.EpollEvent, 16)
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		n, err := unix.EpollWait(w.epfd, events, 500)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		if n > 0 {
			select {
			case w.events <- struct{}{}:
			default:
			}
		}
	}
}

func (w *pidfdWatcher) Close() error {
	close(w.stop)
	<-w.done
	for id, process := range w.watched {
		_ = unix.Close(process.fd)
		delete(w.watched, id)
	}
	return unix.Close(w.epfd)
}
