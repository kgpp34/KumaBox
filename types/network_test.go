package types

import "testing"

func TestNetworkSetupValidatesDurableHandoff(t *testing.T) {
	setup := NetworkSetup{
		Backend:   NetworkBackendCNI,
		Namespace: "/var/run/netns/kb-sandbox",
		Interfaces: []NetworkInterface{{
			Index: 0, Name: "eth0", TAP: "tap12345678-0", MAC: "02:00:00:00:00:01",
			Queues: 4, QueueSize: 512, Network: "bridge",
			IPv4: &IPv4Config{Address: "10.42.0.7", Gateway: "10.42.0.1", Prefix: 24},
		}},
	}
	if err := setup.Validate(); err != nil {
		t.Fatal(err)
	}
	setup.Interfaces = append(setup.Interfaces, setup.Interfaces[0])
	if err := setup.Validate(); err == nil {
		t.Fatal("duplicate network interface was accepted")
	}
}

func TestNetworkSetupZeroValueDisablesNetworking(t *testing.T) {
	if err := (NetworkSetup{}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (NetworkSetup{Namespace: "/var/run/netns/unowned"}).Validate(); err == nil {
		t.Fatal("namespace without backend was accepted")
	}
}

func TestNetworkSetupRejectsNonContiguousInterfaceIndices(t *testing.T) {
	setup := NetworkSetup{
		Backend:   NetworkBackendCNI,
		Namespace: "/var/run/netns/kb-sandbox",
		Interfaces: []NetworkInterface{{
			Index: 1, Name: "eth1", TAP: "tap12345678-1", MAC: "02:00:00:00:00:02",
			Queues: 2, QueueSize: 512, Network: "bridge",
		}},
	}
	if err := setup.Validate(); err == nil {
		t.Fatal("NetworkSetup accepted an interface sequence that does not begin at zero")
	}
}
