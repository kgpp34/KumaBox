//go:build linux

package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func TestGuestUserSelectionUsesCredentialsAndUserEnvironment(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching guest credentials requires root")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("test guest has no nobody account")
	}
	for _, selected := range []string{account.Username, account.Uid} {
		t.Run(selected, func(t *testing.T) {
			server := processServer(t)
			var output bytes.Buffer
			code, err := Run(t.Context(), processConnection(t, server), types.Command{
				Args: []string{"sh", "-c", `id -u; id -g; printf '%s\n%s\n%s' "$USER" "$LOGNAME" "$HOME"`}, User: selected,
			}, nil, &output, io.Discard)
			want := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", account.Uid, account.Gid, account.Username, account.Username, account.HomeDir)
			if err != nil || code != 0 || output.String() != want {
				t.Fatalf("guest identity: code=%d output=%q want=%q err=%v", code, output.String(), want, err)
			}
			info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", `id -u; printf '%s' "$HOME"`}, User: selected, Env: map[string]string{"HOME": "/explicit-home"}})
			output.Reset()
			_, err = AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, &output, io.Discard)
			if err != nil || output.String() != account.Uid+"\n/explicit-home" {
				t.Fatalf("detached identity/override = %q, %v", output.String(), err)
			}
		})
	}
}

func TestSignalingBackgroundCommandTerminatesItsProcessGroup(t *testing.T) {
	server := processServer(t)
	info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", `sleep 30 & echo "$!"; wait`}})
	observer := processConnection(t, server)
	if err := NewEncoder(observer).Encode(Message{Type: MessageAttach, ProcessID: info.ID}); err != nil {
		t.Fatal(err)
	}
	message, err := NewDecoder(observer).Decode()
	if err != nil || message.Type != MessageStdout {
		t.Fatalf("child PID output = %+v, %v", message, err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(message.Data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child PID %q", message.Data)
	}
	_ = observer.Close()
	if err := SignalProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessSignalKILL); err != nil {
		t.Fatal(err)
	}
	result, err := AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, io.Discard, io.Discard)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 137 {
		t.Fatalf("group exit = %+v, %v", result, err)
	}
	// SIGKILL queues termination; reaping the group leader does not prove that
	// every descendant has been scheduled to exit. Wait for observable exit,
	// accepting zombies because a container's PID 1 may not reap adopted children.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", childPID))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		_, fields, ok := strings.Cut(string(stat), ") ")
		if !ok {
			t.Fatalf("invalid descendant process status: %s", stat)
		}
		if strings.HasPrefix(fields, "Z ") {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("background descendant survived SIGKILL: %s", stat)
		}
	}
}
