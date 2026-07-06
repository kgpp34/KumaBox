//go:build !linux

package network

import (
	"fmt"
	"runtime"
)

func AttachHostTap(_ Record) error {
	return fmt.Errorf("host-tap attach requires Linux (running on %s)", runtime.GOOS)
}

func DeleteHostTap(_ string) error {
	return fmt.Errorf("host-tap delete requires Linux (running on %s)", runtime.GOOS)
}
