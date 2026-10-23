//go:build linux

package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConsoleRelayInheritedFDEntry(t *testing.T) {
	if os.Getenv("KUMABOX_TEST_CONSOLE_RELAY") != "1" {
		return
	}
	if err := RunConsoleRelay(context.Background()); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestConsoleRelaySurvivesDetachAndExitsWithVMM(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kb-console-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "console.sock")
	console, err := prepareConsole(path)
	if err != nil {
		t.Fatal(err)
	}
	defer console.close()
	vmm := exec.Command("sleep", "30")
	if err := vmm.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- vmm.Wait() }()
	t.Cleanup(func() { _ = vmm.Process.Kill() })
	fd, err := unix.PidfdOpen(vmm.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	lifetime := os.NewFile(uintptr(fd), "VMM pidfd")
	defer lifetime.Close() //nolint:errcheck
	listener, err := console.listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	ready, ack, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close() //nolint:errcheck
	defer ack.Close()   //nolint:errcheck
	log, err := os.Create(filepath.Join(dir, "serial.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close() //nolint:errcheck
	child := exec.Command(os.Args[0], "-test.run=^TestConsoleRelayInheritedFDEntry$")
	child.Env = append(os.Environ(), "KUMABOX_TEST_CONSOLE_RELAY=1")
	child.ExtraFiles = []*os.File{console.master, listener, lifetime, ack, log}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childExit := make(chan error, 1)
	go func() { childExit <- child.Wait() }()
	t.Cleanup(func() { _ = child.Process.Kill() })
	_ = ack.Close()
	// A pipe deadline bounds failure even if the helper never initializes.
	_ = ready.SetReadDeadline(time.Now().Add(5 * time.Second))
	var response [1]byte
	if _, err := io.ReadFull(ready, response[:]); err != nil || response[0] != consoleReady {
		t.Fatalf("helper ready = %v %v", response, err)
	}
	// Close the parent's master/listener; only the detached helper owns them.
	_ = console.master.Close()
	_ = console.listener.Close()
	for round := range 2 {
		client, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(client, response[:]); err != nil || response[0] != consoleReady {
			t.Fatalf("attach %d: %v %v", round, response, err)
		}
		if _, err := console.slave.Write([]byte("guest\n")); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 6)
		if _, err := io.ReadFull(client, data); err != nil || string(data) != "guest\n" {
			t.Fatalf("guest serial = %q %v", data, err)
		}
		if _, err := client.Write([]byte("host\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(console.slave, data[:5]); err != nil || string(data[:5]) != "host\n" {
			t.Fatalf("host serial = %q %v", data[:5], err)
		}
		_ = client.Close()
		// A fresh connection can briefly be busy until the input goroutine sees EOF.
		deadline := time.Now().Add(3 * time.Second)
		for {
			probe, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			_ = probe.SetDeadline(time.Now().Add(time.Second))
			_, err = io.ReadFull(probe, response[:])
			_ = probe.Close()
			if err == nil && response[0] == consoleReady {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("console did not become attachable")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := vmm.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	select {
	case err := <-childExit:
		if err != nil {
			t.Fatalf("helper exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console helper outlived VMM")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("helper left console socket: %v", err)
	}
}

func TestConsolePIDFDWatchCancellationDoesNotKillVMM(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	fd, err := unix.PidfdOpen(command.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd) //nolint:errcheck
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- waitConsoleProcessExit(ctx, fd) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pidfd watcher failed to cancel")
	}
	if err := command.Process.Signal(unix.Signal(0)); err != nil {
		t.Fatalf("cancellation killed VMM: %v", err)
	}
}
