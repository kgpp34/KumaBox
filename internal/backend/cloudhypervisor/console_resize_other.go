//go:build !linux

package cloudhypervisor

import "fmt"

func setConsoleSize(_ uintptr, _, _ uint16) error {
	return fmt.Errorf("console resize is only supported on Linux")
}
