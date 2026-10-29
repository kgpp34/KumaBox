package agent

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineIDUsesFreshHostEntropy(t *testing.T) {
	for _, test := range []struct {
		name string
		sync bool
	}{{name: "durable", sync: true}, {name: "writeback"}} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			machineIDPath := filepath.Join(directory, "machine-id")
			if err := os.WriteFile(machineIDPath, []byte("previous\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			seed := bytes.Repeat([]byte{0x42}, reseedEntropyBytes)
			if err := writeMachineIDAt(seed, machineIDPath, test.sync); err != nil {
				t.Fatal(err)
			}
			identity, err := os.ReadFile(machineIDPath)
			if err != nil || len(identity) != 33 || identity[32] != '\n' || strings.Contains(string(identity), "previous") {
				t.Fatalf("machine ID = %q, %v", identity, err)
			}
			if err := writeMachineIDAt(seed[:31], machineIDPath, test.sync); err == nil {
				t.Fatal("short entropy was accepted")
			}
			if err := writeMachineIDAt(bytes.Repeat([]byte{0x43}, reseedEntropyBytes), machineIDPath, test.sync); err != nil {
				t.Fatal(err)
			}
			next, err := os.ReadFile(machineIDPath)
			if err != nil || bytes.Equal(next, identity) {
				t.Fatalf("next clone reused identity %q: %v", next, err)
			}
		})
	}
}

func TestReseedWritebackStillWaitsForAcknowledgment(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	done := make(chan error, 1)
	go func() {
		done <- ReseedWithOptions(t.Context(), host, bytes.Repeat([]byte{7}, reseedEntropyBytes), ReseedOptions{
			RegenerateMachineID: true, SkipMachineIDSync: true,
		})
	}()
	request, err := NewDecoder(guest).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if !request.RegenMachineID || !request.SkipMachineIDSync {
		t.Fatalf("initialization policy was lost: %+v", request)
	}
	select {
	case err := <-done:
		t.Fatalf("reseed returned before initialization acknowledgement: %v", err)
	default:
	}
	if err := NewEncoder(guest).Encode(Message{Type: MessageError, Message: "identity write failed"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "identity write failed") {
		t.Fatalf("initialization rejection was hidden: %v", err)
	}
}

func TestReseedWritebackCanContactOlderAgent(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	done := make(chan error, 1)
	go func() {
		// This is the original request schema; unknown fields are ignored.
		var request struct {
			Type           string `json:"type"`
			Data           []byte `json:"data"`
			RegenMachineID bool   `json:"regen_machine_id"`
		}
		if err := json.NewDecoder(guest).Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Type != MessageReseed || !request.RegenMachineID || len(request.Data) != reseedEntropyBytes {
			done <- errMissingExit
			return
		}
		done <- NewEncoder(guest).Encode(Message{Type: MessageExit})
	}()
	if err := ReseedWithOptions(t.Context(), host, bytes.Repeat([]byte{7}, reseedEntropyBytes), ReseedOptions{
		RegenerateMachineID: true, SkipMachineIDSync: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReseedClientWaitsForAgentAcknowledgment(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = guest.Close() }()
		message, err := NewDecoder(guest).Decode()
		if err != nil {
			done <- err
			return
		}
		if message.Type != MessageReseed || len(message.Data) != reseedEntropyBytes || !message.RegenMachineID {
			done <- errMissingExit
			return
		}
		done <- NewEncoder(guest).Encode(Message{Type: MessageExit})
	}()
	if err := Reseed(t.Context(), host, bytes.Repeat([]byte{7}, reseedEntropyBytes), true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
