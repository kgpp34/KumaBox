//go:build linux

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	guestRandomDevice = "/dev/urandom"
	savedRandomSeed   = "/var/lib/systemd/random-seed"
	guestMachineID    = "/etc/machine-id"
	dbusMachineID     = "/var/lib/dbus/machine-id"
)

// reseedGuest injects host entropy, forces a guest CRNG reseed, and discards
// random state persisted by systemd before the snapshot was captured.
func reseedGuest(entropy []byte, regenerateMachineID bool) error {
	if len(entropy) != reseedEntropyBytes {
		return fmt.Errorf("reseed needs %d entropy bytes", reseedEntropyBytes)
	}
	defer clear(entropy)
	var errs []error
	if err := reseedKernel(entropy); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(savedRandomSeed); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove saved random seed: %w", err))
	}
	if regenerateMachineID {
		if err := writeMachineIDAt(entropy, guestMachineID); err != nil {
			errs = append(errs, err)
		}
		if err := dropStaleDBusMachineID(dbusMachineID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func reseedKernel(entropy []byte) error {
	fd, err := unix.Open(guestRandomDevice, unix.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open guest random device: %w", err)
	}
	buffer := encodeEntropy(entropy)
	defer clear(buffer)
	var errs []error
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&buffer[0]))); errno != 0 { //nolint:gosec // the ioctl ABI requires a pointer to rand_pool_info
		errs = append(errs, fmt.Errorf("add guest entropy: %w", errno))
	}
	runtime.KeepAlive(buffer)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.RNDRESEEDCRNG, 0); errno != 0 {
		errs = append(errs, fmt.Errorf("reseed guest CRNG: %w", errno))
	}
	if err := unix.Close(fd); err != nil {
		errs = append(errs, fmt.Errorf("close guest random device: %w", err))
	}
	return errors.Join(errs...)
}

// encodeEntropy creates the Linux rand_pool_info header followed by the seed.
func encodeEntropy(entropy []byte) []byte {
	buffer := make([]byte, 8+len(entropy))
	binary.NativeEndian.PutUint32(buffer[:4], uint32(len(entropy))*8) //nolint:gosec // protocol length is fixed at 32 bytes
	binary.NativeEndian.PutUint32(buffer[4:8], uint32(len(entropy)))  //nolint:gosec // protocol length is fixed at 32 bytes
	copy(buffer[8:], entropy)
	return buffer
}

func dropStaleDBusMachineID(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return os.Remove(path)
	}
	return nil
}
