//go:build !linux

package guestagent

import "fmt"

func applyIdentity(identityRequest) error {
	return fmt.Errorf("identity configuration is only supported on Linux")
}
