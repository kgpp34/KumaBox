package agent

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineIDUsesFreshHostEntropy(t *testing.T) {
	directory := t.TempDir()
	machineIDPath := filepath.Join(directory, "machine-id")
	if err := os.WriteFile(machineIDPath, []byte("previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seed := bytes.Repeat([]byte{0x42}, reseedEntropyBytes)
	if err := writeMachineIDAt(seed, machineIDPath); err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(machineIDPath)
	if err != nil || len(identity) != 33 || identity[32] != '\n' || strings.Contains(string(identity), "previous") {
		t.Fatalf("machine ID = %q, %v", identity, err)
	}
	if err := writeMachineIDAt(seed[:31], machineIDPath); err == nil {
		t.Fatal("short entropy was accepted")
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
