//go:build linux

package vmm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/storage"
)

func TestCommandLineMatchesDistinguishesPendingIdentity(t *testing.T) {
	const socket = "/run/kumabox/api.sock"
	if match, err := commandLineMatches(nil, "cloud-hypervisor", socket); match || !errors.Is(err, ErrProcessIdentityPending) {
		t.Fatalf("empty command line = %v, %v; want pending identity", match, err)
	}
	if match, err := commandLineMatches([]byte("cloud-hypervisor\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || !match {
		t.Fatalf("complete command line = %v, %v; want match", match, err)
	}
	if match, err := commandLineMatches([]byte("other-vmm\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || match {
		t.Fatalf("foreign command line = %v, %v; want mismatch", match, err)
	}
}

func TestCommandLineMatchesFirecrackerSocketIdentity(t *testing.T) {
	const socket = "/run/kumabox/api.sock"
	for _, test := range []struct {
		args string
		want bool
	}{
		{"firecracker\x00--api-sock\x00" + socket + "\x00", true},
		{"firecracker\x00--api-sock\x00/foreign/api.sock\x00", false},
		{"firecracker\x00--api-sock\x00" + socket + "\x00--api-socket\x00" + socket + "\x00", false},
	} {
		if got, err := commandLineMatches([]byte(test.args), "firecracker", socket); err != nil || got != test.want {
			t.Fatalf("identity: %v %v", got, err)
		}
	}
}

func TestCommandIdentityOfExitedProcessIsAbsent(t *testing.T) {
	command := exec.Command("true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	// Cleanup may read starttime just before a relay exits and lose cmdline
	// immediately afterwards. That final read must report absence, not fail it.
	match, err := processCommandMatches(command.Process.Pid, "true", "/gone/api.sock")
	if err != nil || match {
		t.Fatalf("exited process identity = %v %v", match, err)
	}
}

func TestClearWaitsForExitingConsoleHelper(t *testing.T) {
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Prepare(testSandboxID); err != nil {
		t.Fatal(err)
	}
	socket, _ := paths.APISocket(testSandboxID)
	input, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()    //nolint:errcheck
	defer keepOpen.Close() //nolint:errcheck
	command := exec.Command("/bin/sh", "-c", "read line", "--api-socket", socket)
	command.Stdin = input
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var reaped chan struct{}
	defer func() {
		_ = command.Process.Kill()
		if reaped != nil {
			<-reaped
		} else {
			_ = command.Wait()
		}
	}()
	helper, err := CaptureProcess(command.Process.Pid, testSandboxID, 3, "sh", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.WriteConsoleProcess(helper); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", command.Process.Pid))
		if err == nil && len(raw) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not enter exiting state")
		}
		time.Sleep(time.Millisecond)
	}
	reaped = make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); _ = command.Wait(); close(reaped) }()
	if err := paths.Clear(t.Context(), testSandboxID); err != nil {
		t.Fatalf("exiting helper blocked cleanup: %v", err)
	}
	<-reaped
}
