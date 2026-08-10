package network

// VerifyConfig checks whether the host-side objects required by a persisted
// VM network attachment still exist and are usable.
func VerifyConfig(config Config) error {
	return verifyConfig(config)
}
