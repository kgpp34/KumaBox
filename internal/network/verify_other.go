//go:build !linux

package network

func verifyConfig(_ Config) error {
	// Host network objects only exist on Linux. Other platforms still use fake
	// backends in unit tests; the real backend rejects them before VM launch.
	return nil
}
