package core

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/types"
)

func TestCloneGuestScriptUsesNewIdentityAndAddress(t *testing.T) {
	record := types.Sandbox{
		Config: types.SandboxConfig{Name: "clone-box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage, NICs: 1, NetworkName: "test"},
		Network: types.NetworkSetup{Interfaces: []types.NetworkInterface{{
			Index: 0, Name: "eth0", TAP: "new-tap", MAC: "02:00:00:00:00:02",
			Queues: 4, QueueSize: 512, Network: "test",
			IPv4: &types.IPv4Config{Address: "10.0.0.3", Gateway: "10.0.0.1", Prefix: 24},
		}}},
	}
	script, err := cloneGuestScript(record, []string{"1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"rm -f /etc/systemd/network/10-kumabox-*.network",
		"MACAddress=02:00:00:00:00:02", "Address=10.0.0.3/24", "Gateway=10.0.0.1",
		"DNS=1.1.1.1", "hostname 'clone-box'", "systemctl restart systemd-networkd",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("guest script misses %q: %s", expected, script)
		}
	}
}
