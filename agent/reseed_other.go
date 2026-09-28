//go:build !linux

package agent

import "errors"

func reseedGuest([]byte, bool) error {
	return errors.New("guest reseed requires Linux RNG ioctls")
}
