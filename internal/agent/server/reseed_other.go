//go:build !linux

package server

import "fmt"

const agentReseedEntropyBytes = 32

func applyReseed(reseedRequest) error {
	return fmt.Errorf("reseed is only supported on Linux")
}
