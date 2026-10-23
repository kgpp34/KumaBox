//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/moby/term"
	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/vmm"
)

// The private self-exec ABI transfers already-open resources; the helper never
// resolves host paths or parses environment-based configuration.
const (
	relayMasterFD = 3 + iota
	relayListenerFD
	relayPIDFD
	relayReadyFD
	relayLogFD
)

// consoleLaunch owns the parent's descriptors until the helper acknowledges
// takeover. The VMM inherits only the raw slave; the helper inherits the master.
type consoleLaunch struct {
	master   *os.File
	slave    *os.File
	listener *net.UnixListener
}

func (c *consoleLaunch) close() {
	_ = c.master.Close()
	_ = c.slave.Close()
	_ = c.listener.Close()
}

func prepareConsole(socket string) (*consoleLaunch, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = master.Close()
		}
	}()
	raw, err := master.SyscallConn()
	if err != nil {
		return nil, err
	}
	var index int
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0)
		if ioctlErr == nil {
			index, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN)
		}
	}); err != nil {
		return nil, err
	}
	if ioctlErr != nil {
		return nil, fmt.Errorf("prepare console PTY: %w", ioctlErr)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", index), os.O_RDWR|syscall.O_NOCTTY, 0) //nolint:gosec // index comes from the kernel PTY ioctl
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			_ = slave.Close()
		}
	}()
	if _, err := term.SetRawTerminal(slave.Fd()); err != nil {
		return nil, fmt.Errorf("set console PTY raw: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Closing the parent's copy must not unlink the helper's inherited listener.
	listener.SetUnlinkOnClose(false)
	ok = true
	return &consoleLaunch{master: master, slave: slave, listener: listener}, nil
}

// startConsoleRelay detaches a helper outside the VMM cgroup. That cgroup must
// contain only the actual VMM for identity recovery and CPU accounting. A pidfd
// binds helper lifetime to this exact VMM, even after the launching CLI exits.
func (d *Driver) startConsoleRelay(ctx context.Context, process vmm.Process, console *consoleLaunch, log *os.File) (returnErr error) {
	fd, err := unix.PidfdOpen(process.PID, 0)
	if err != nil {
		return fmt.Errorf("open console lifetime pidfd: %w", err)
	}
	pidfd := os.NewFile(uintptr(fd), "VMM lifetime")
	defer pidfd.Close() //nolint:errcheck // parent closes only its duplicated descriptor
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("VMM exited before console relay startup")
	}
	listener, err := console.listener.File()
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // inherited listener remains owned by the helper
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyReader.Close() //nolint:errcheck // pipe serves only the startup acknowledgement
	defer readyWriter.Close() //nolint:errcheck
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, ConsoleRelayCommand, "--api-socket", process.APISocket) //nolint:gosec // self-exec with fixed inherited-FD entrypoint
	command.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=32MiB"}
	command.ExtraFiles = []*os.File{console.master, listener, pidfd, readyWriter, log}
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	defer func() {
		if returnErr != nil {
			_ = command.Process.Kill()
			<-exited
		}
	}()
	_ = readyWriter.Close()
	helper, err := vmm.CaptureProcess(command.Process.Pid, process.SandboxID, process.Generation, filepath.Base(executable), process.APISocket)
	if err != nil {
		return err
	}
	if err := d.paths.WriteConsoleProcess(helper); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, d.options.StartupTimeout)
	defer cancel()
	stop := context.AfterFunc(wait, func() { _ = readyReader.Close() })
	defer stop()
	var response [1]byte
	if _, err := io.ReadFull(readyReader, response[:]); err != nil {
		if wait.Err() != nil {
			return wait.Err()
		}
		return fmt.Errorf("console relay readiness: %w", err)
	}
	if response[0] != consoleReady {
		return errors.New("invalid console relay readiness")
	}
	alive, err = vmm.VerifyProcess(helper)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("console relay exited during startup")
	}
	return nil
}

// RunConsoleRelay serves inherited PTY/listener descriptors until the exact VMM
// exits or this helper receives a termination signal. It joins the pidfd watcher
// and serial workers before unlinking only its own socket inode.
func RunConsoleRelay(parent context.Context) (returnErr error) {
	// exec transfers a blocking descriptor. Register it with Go's poller only
	// after restoring O_NONBLOCK, so cancellation can interrupt PTY reads/writes.
	if _, err := unix.IoctlGetInt(relayMasterFD, unix.TIOCGPTN); err != nil {
		return fmt.Errorf("invalid console PTY: %w", err)
	}
	if err := unix.SetNonblock(relayMasterFD, true); err != nil {
		return err
	}
	master := os.NewFile(relayMasterFD, "serial PTY")
	listenerFile := os.NewFile(relayListenerFD, "console listener")
	pidfd := os.NewFile(relayPIDFD, "VMM lifetime")
	ready := os.NewFile(relayReadyFD, "relay readiness")
	log := os.NewFile(relayLogFD, "serial log")
	defer func() {
		_ = master.Close()
		_ = listenerFile.Close()
		_ = pidfd.Close()
		_ = ready.Close()
		_ = log.Close()
	}()
	// Reject manual invocations without the required inherited descriptors.
	for _, file := range []*os.File{master, listenerFile, pidfd, ready, log} {
		if _, err := file.Stat(); err != nil {
			return fmt.Errorf("invalid console relay descriptor: %w", err)
		}
	}
	listener, err := net.FileListener(listenerFile)
	if err != nil {
		return err
	}
	_ = listenerFile.Close()
	defer listener.Close() //nolint:errcheck // serialRelay also closes the listener
	socket := listener.Addr().String()
	original, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	defer func() {
		current, err := os.Lstat(socket)
		if err == nil && os.SameFile(original, current) {
			_ = os.Remove(socket)
		} //nolint:gosec // socket was inherited, and inode ownership is rechecked
	}()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watched := make(chan error, 1)
	go func() { watched <- waitConsoleProcessExit(ctx, int(pidfd.Fd())); cancel() }()
	relay := &serialRelay{serial: master, listener: listener, log: log}
	serveErr := relay.serve(ctx, ready)
	cancel()
	watchErr := <-watched
	return errors.Join(serveErr, watchErr)
}

// waitConsoleProcessExit blocks in the kernel rather than polling every VM.
// An eventfd wakes the watcher on cancellation without reusing a closed pidfd.
func waitConsoleProcessExit(ctx context.Context, pidfd int) error {
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return err
	}
	wakeFile := os.NewFile(uintptr(wake), "console cancel")
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _, _ = wakeFile.Write([]byte{1, 0, 0, 0, 0, 0, 0, 0}); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
		_ = wakeFile.Close()
	}()
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}} //nolint:gosec // kernel file descriptors fit signed 32-bit
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		if fds[0].Revents&(unix.POLLNVAL|unix.POLLERR|unix.POLLHUP) != 0 {
			return errors.New("invalid console lifetime pidfd")
		}
	}
}
