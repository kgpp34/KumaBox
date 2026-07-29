//go:build linux

package server

import "testing"

func TestApplyNetworkIdentitySkipsNetworklessClone(t *testing.T) {
	originalStateDir := networkdStateDir
	originalRun := runNetworkctl
	networkdStateDir = t.TempDir()
	runNetworkctl = func(args ...string) error {
		t.Fatalf("networkctl unexpectedly invoked: %v", args)
		return nil
	}
	t.Cleanup(func() {
		networkdStateDir = originalStateDir
		runNetworkctl = originalRun
	})

	if err := applyNetworkIdentity(nil); err != nil {
		t.Fatalf("apply networkless identity: %v", err)
	}
}
