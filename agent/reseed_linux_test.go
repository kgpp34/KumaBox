//go:build linux

package agent

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestEntropyIoctlPayload(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, reseedEntropyBytes)
	buffer := encodeEntropy(seed)
	if binary.NativeEndian.Uint32(buffer[:4]) != 256 || binary.NativeEndian.Uint32(buffer[4:8]) != reseedEntropyBytes || !bytes.Equal(buffer[8:], seed) {
		t.Fatalf("ioctl payload = %x", buffer)
	}
}

func TestDropStaleDBusMachineIDPreservesSymlink(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "regular")
	linked := filepath.Join(directory, "linked")
	if err := os.WriteFile(regular, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/machine-id", linked); err != nil {
		t.Fatal(err)
	}
	if err := dropStaleDBusMachineID(regular); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regular); !os.IsNotExist(err) {
		t.Fatalf("stale regular machine ID remained: %v", err)
	}
	if err := dropStaleDBusMachineID(linked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(linked); err != nil {
		t.Fatal(err)
	}
}
