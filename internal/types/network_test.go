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

func TestNetworkSetupAcceptsBridgeWithoutNamespace(t *testing.T) {
	setup := NetworkSetup{
		Backend: NetworkBackendBridge,
		Interfaces: []NetworkInterface{{
			Index: 0, Name: "eth0", TAP: "bt12345678-0", MAC: "02:00:00:00:00:01",
			Queues: 4, QueueSize: 512, Network: "br0",
		}},
	}
	if err := setup.Validate(); err != nil {
		t.Fatal(err)
	}
	setup.Namespace = "/var/run/netns/unexpected"
	if err := setup.Validate(); err == nil {
		t.Fatal("bridge setup with a namespace was accepted")
	}
}

func TestFixedGuestIdentityRequiresSeparateHostAddressAndEBPF(t *testing.T) {
	setup := NetworkSetup{Backend: NetworkBackendEBPF, Interfaces: []NetworkInterface{{
		Index: 0, Name: "eth0", TAP: "kt12345678-0", MAC: "02:4b:42:00:00:02", Queues: 4, QueueSize: 512, Network: "ebpf",
		AddressMode: AddressModeFixed, HostAddress: "10.89.0.7", IPv4: &IPv4Config{Address: "169.254.240.2", Gateway: "169.254.240.1", Prefix: 30},
	}}}
	if err := setup.Validate(); err != nil {
		t.Fatal(err)
	}
	setup.Interfaces[0].HostAddress = setup.Interfaces[0].IPv4.Address
	if err := setup.Validate(); err == nil {
		t.Fatal("overlapping guest and host identities accepted")
	}
	setup.Interfaces[0].HostAddress = "10.89.0.7"
	setup.Backend = NetworkBackendBridge
	if err := setup.Validate(); err == nil {
		t.Fatal("fixed shared identity on a bridge was accepted")
	}
}
