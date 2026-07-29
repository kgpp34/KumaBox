//go:build linux

package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

// Linux does not expose these PTY ioctls from x/sys on every supported
// version, so keep the kernel ABI values together with the PTY implementation.
const (
	ptyGetNumber = 0x80045430
	ptyUnlock    = 0x40045431
)

func handleTTYExec(reader *bufio.Reader, rw io.ReadWriter, request protocol.Frame) {
	if err := validateUser(request.User); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorUserUnsupported, err.Error())
		return
	}
	if err := validateEnvironment(request.Env, agentPolicy.deniedEnv); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorEnvDenied, err.Error())
		return
	}
	master, slave, err := openPTY(request.Rows, request.Columns)
	if err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, fmt.Sprintf("open PTY: %v", err))
		return
	}
	defer master.Close() //nolint:errcheck
	defer slave.Close()  //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), agentPolicy.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, request.Args[0], request.Args[1:]...) //nolint:gosec
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = request.WorkDir
	cmd.Env = mergeEnvironment(request.Env)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Ctty is an index into the child's stdin/stdout/stderr file list, not
	// the parent's PTY file descriptor.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setpgid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, err.Error())
		return
	}
	_ = slave.Close()

	writer := &streamWriter{writer: rw}
	if err := writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameReady, ID: request.ID}); err != nil {
		_ = cmd.Process.Kill()
		return
	}

	frames := make(chan protocol.Frame, 1)
	frameErrs := make(chan error, 1)
	go func() {
		decoder := protocol.NewDecoder(reader)
		for {
			frame, readErr := decoder.ReadFrame()
			if readErr != nil {
				frameErrs <- readErr
				return
			}
			frames <- frame
		}
	}()

	outputErrs := make(chan error, 1)
	outputDone := make(chan error, 1)
	output := &outputBudget{limit: agentPolicy.maxOutput}
	go func() {
		buf := make([]byte, streamChunkSize)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				if _, writeErr := (&streamOutputWriter{writer: writer, id: request.ID, frameType: protocol.FrameStdout, stream: protocol.StreamStdout, budget: output}).Write(buf[:n]); writeErr != nil {
					outputErrs <- writeErr
					outputDone <- writeErr
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, syscall.EIO) || errors.Is(readErr, io.EOF) {
					outputDone <- nil
				} else {
					outputErrs <- readErr
					outputDone <- readErr
				}
				return
			}
		}
	}()

	waitErrs := make(chan error, 1)
	go func() { waitErrs <- cmd.Wait() }()
	for {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
			if ctx.Err() == context.DeadlineExceeded {
				writeStreamError(rw, request.ID, protocol.ErrorExecTimeout, "execution exceeded policy timeout")
			}
			return
		case frame := <-frames:
			if frame.ID != request.ID {
				_ = cmd.Process.Kill()
				return
			}
			switch frame.Type {
			case protocol.FrameStdin:
				data := frame.Data
				if frame.End {
					data = append(data, 4) // terminal EOF (Ctrl-D)
				}
				if _, err := master.Write(data); err != nil {
					_ = cmd.Process.Kill()
					return
				}
			case protocol.FrameResize:
				if err := resizePTY(master, frame.Rows, frame.Columns); err != nil {
					writeStreamError(rw, request.ID, protocol.ErrorInvalidRequest, err.Error())
				}
			case protocol.FrameSignal:
				if err := signalProcess(cmd, frame.Signal); err != nil {
					writeStreamError(rw, request.ID, protocol.ErrorInvalidRequest, err.Error())
				}
			default:
				_ = cmd.Process.Kill()
				return
			}
		case <-frameErrs:
			_ = cmd.Process.Kill()
			return
		case outputErr := <-outputErrs:
			if outputErr != nil {
				killProcessTree(cmd)
				if output.exceeded() {
					writeStreamError(rw, request.ID, protocol.ErrorOutputLimit, "execution output exceeded policy limit")
				}
				return
			}
		case waitErr := <-waitErrs:
			<-outputDone
			exitCode := 0
			if waitErr != nil {
				exitCode = commandExitCode(waitErr)
			}
			_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameExit, ID: request.ID, ExitCode: exitCode})
			return
		}
	}
}

func openPTY(rows, columns uint16) (*os.File, *os.File, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	closeMaster := true
	defer func() {
		if closeMaster {
			_ = unix.Close(masterFD)
		}
	}()
	if err := unix.IoctlSetPointerInt(masterFD, ptyUnlock, 0); err != nil {
		return nil, nil, err
	}
	ptyNumber, err := unix.IoctlGetInt(masterFD, ptyGetNumber)
	if err != nil {
		return nil, nil, err
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", ptyNumber), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
	slave := os.NewFile(uintptr(slaveFD), fmt.Sprintf("/dev/pts/%d", ptyNumber))
	if err := resizePTY(master, rows, columns); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, err
	}
	closeMaster = false
	return master, slave, nil
}

func resizePTY(master *os.File, rows, columns uint16) error {
	if rows == 0 || columns == 0 {
		return fmt.Errorf("PTY dimensions must be non-zero")
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: columns})
}

func signalProcess(cmd *exec.Cmd, name string) error {
	var signal syscall.Signal
	switch strings.ToUpper(strings.TrimPrefix(name, "SIG")) {
	case "INT":
		signal = syscall.SIGINT
	case "TERM":
		signal = syscall.SIGTERM
	case "HUP":
		signal = syscall.SIGHUP
	case "WINCH":
		signal = syscall.SIGWINCH
	case "QUIT":
		signal = syscall.SIGQUIT
	default:
		return fmt.Errorf("unsupported signal %q", name)
	}
	return cmd.Process.Signal(signal)
}
