//go:build linux

package server

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	agentReseedEntropyBytes = 32
	machineIDBytes          = 16

	urandomPath       = "/dev/urandom"
	systemdRandomSeed = "/var/lib/systemd/random-seed"
	machineIDPath     = "/etc/machine-id"
	dbusMachineIDPath = "/var/lib/dbus/machine-id"
)

func applyReseed(req reseedRequest) error {
	var errs []error
	if err := reseedKernel(req.Entropy); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(systemdRandomSeed); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove systemd random seed: %w", err))
	}
	if req.RegenerateMachineID {
		if err := regenerateMachineID(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func reseedKernel(entropy []byte) error {
	fd, err := unix.Open(urandomPath, unix.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", urandomPath, err)
	}
	var errs []error
	if err := addKernelEntropy(fd, entropy); err != nil {
		errs = append(errs, err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.RNDRESEEDCRNG, 0); errno != 0 {
		errs = append(errs, fmt.Errorf("reseed CRNG: %w", errno))
	}
	if err := unix.Close(fd); err != nil {
		errs = append(errs, fmt.Errorf("close %s: %w", urandomPath, err))
	}
	return errors.Join(errs...)
}

func addKernelEntropy(fd int, entropy []byte) error {
	buffer := make([]byte, 8+len(entropy))
	defer clear(buffer)
	binary.NativeEndian.PutUint32(buffer[0:4], uint32(len(entropy)*8)) //nolint:gosec // request size is fixed at 32 bytes
	binary.NativeEndian.PutUint32(buffer[4:8], uint32(len(entropy)))   //nolint:gosec // request size is fixed at 32 bytes
	copy(buffer[8:], entropy)
	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		unix.RNDADDENTROPY,
		uintptr(unsafe.Pointer(&buffer[0])), //nolint:gosec // ioctl requires rand_pool_info memory layout
	); errno != 0 {
		return fmt.Errorf("add entropy: %w", errno)
	}
	return nil
}

func regenerateMachineID() error {
	if _, err := os.Stat(machineIDPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", machineIDPath, err)
	}
	id, err := randomMachineID()
	if err != nil {
		return err
	}
	if err := os.WriteFile(machineIDPath, []byte(id), 0o444); err != nil { //nolint:gosec // machine-id is conventionally world-readable
		return fmt.Errorf("write %s: %w", machineIDPath, err)
	}
	if err := dropStaleDBusMachineID(dbusMachineIDPath); err != nil {
		auditLog.Printf("reseed warning: drop stale D-Bus machine ID: %v", err)
	}
	return nil
}

func randomMachineID() (string, error) {
	raw := make([]byte, machineIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate machine ID: %w", err)
	}
	return hex.EncodeToString(raw) + "\n", nil
}

func dropStaleDBusMachineID(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
