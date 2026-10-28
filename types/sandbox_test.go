package types

import (
	"strings"
	"testing"
	"time"
)

func TestSandboxID(t *testing.T) {
	id, err := NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseSandboxID(id.String()); err != nil || parsed != id {
		t.Fatalf("ParseSandboxID(%q) = %q, %v", id, parsed, err)
	}
	if id.String()[14] != '4' || !strings.ContainsRune("89ab", rune(id.String()[19])) {
		t.Fatalf("ID %q is not UUIDv4", id)
	}
}

func TestSandboxConfigValidationMatchesCreateContract(t *testing.T) {
	valid := SandboxConfig{Name: "agent.demo-1", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		config SandboxConfig
	}{
		{"name", SandboxConfig{Name: "bad/name", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage}},
		{"cpus", SandboxConfig{Name: "demo", Memory: MinSandboxMemory, Storage: MinSandboxStorage}},
		{"memory", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory - 1, Storage: MinSandboxStorage}},
		{"storage", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage - 1}},
		{"NIC count", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: MaxSandboxNICs + 1}},
		{"network name", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: 1, NetworkName: "bad/name"}},
		{"bridge without device", SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: 1, NetworkBackend: NetworkBackendBridge}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if err == nil {
				t.Fatal("invalid spec passed validation")
			}
			if strings.Contains(err.Error(), "--") {
				t.Fatalf("domain validation leaked CLI flag syntax: %v", err)
			}
		})
	}
	// A running sandbox may retain its network selection after resizing to zero.
	withRetainedNetwork := valid
	withRetainedNetwork.NetworkName = "default"
	if err := withRetainedNetwork.Validate(); err != nil {
		t.Fatalf("zero-NIC config lost its retained network: %v", err)
	}
	bridge := valid
	bridge.NICs = 1
	bridge.NetworkBackend = NetworkBackendBridge
	bridge.NetworkName = "br0"
	if err := bridge.Validate(); err != nil {
		t.Fatalf("valid bridge config was rejected: %v", err)
	}
	bridge.NICs = 0
	if err := bridge.Validate(); err != nil {
		t.Fatalf("offline bridge policy was rejected: %v", err)
	}
}

func TestWritableRootIOModeIsInheritedAndValidated(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   DiskIOMode
		direct bool
	}{
		{name: "legacy", direct: true},
		{name: "automatic", mode: DiskIOAuto, direct: true},
		{name: "direct", mode: DiskIODirect, direct: true},
		{name: "buffered", mode: DiskIOBuffered},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := SandboxConfig{Name: "box", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, COWIOMode: test.mode}
			if err := config.Validate(); err != nil || config.COWIOMode.DirectIO() != test.direct {
				t.Fatalf("I/O mode %q: direct=%v, err=%v", test.mode, config.COWIOMode.DirectIO(), err)
			}
			other := config
			other.COWIOMode = DiskIOBuffered
			if config.SameGuest(other) != !test.direct {
				t.Fatal("restore accepted a different persisted root-disk I/O policy")
			}
		})
	}
	config := SandboxConfig{Name: "box", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, COWIOMode: "unsafe"}
	if err := config.Validate(); err == nil {
		t.Fatal("unknown I/O policy accepted")
	}
}

func TestCreatingOfflineSandboxCanReserveItsNetworkPolicy(t *testing.T) {
	digest, err := ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewSandboxID()
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []NetworkBackend{NetworkBackendBridge, NetworkBackendEBPF} {
		t.Run(string(backend), func(t *testing.T) {
			record := Sandbox{
				ID: id, Config: SandboxConfig{
					Name: "offline", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage,
					NetworkBackend: backend, NetworkName: "selected",
				}, ImageDigest: digest, VMM: VMMCloudHypervisor,
				State: SandboxStateCreating, Generation: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			if err := record.Validate(); err != nil {
				t.Fatal(err)
			}
			record.State = SandboxStateCreated
			if err := record.Validate(); err == nil {
				t.Fatal("committed network selection without an owner was accepted")
			}
			record.Network = NetworkSetup{Backend: backend, Interfaces: []NetworkInterface{}}
			if err := record.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveDataDisksKeepsInheritedNamesAndAvoidsCollisions(t *testing.T) {
	inherited := []DataDiskSpec{{Name: "data1", Size: MinDataDiskSize, FSType: "ext4"}}
	additions := []DataDiskSpec{{Size: MinDataDiskSize, FSType: "none"}, {Name: "data2", Size: MinDataDiskSize, FSType: "ext4"}}
	resolved, err := ResolveDataDisks(inherited, additions)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 3 || resolved[0].Name != "data1" || resolved[1].Name != "data3" || resolved[2].Name != "data2" {
		t.Fatalf("resolved disks = %+v", resolved)
	}
	if additions[0].Name != "" {
		t.Fatal("resolver mutated caller's additions")
	}
	if _, err := ResolveDataDisks(inherited, []DataDiskSpec{{Name: "data1", Size: MinDataDiskSize, FSType: "ext4"}}); err == nil {
		t.Fatal("new disk reused an inherited serial")
	}
}

func TestCommandValidation(t *testing.T) {
	command := Command{Args: []string{"sh", "-c", "echo"}, Env: map[string]string{"A": "2", "EMPTY": ""}}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Command{
		{},
		{Args: []string{""}},
		{Args: []string{"echo", "bad\x00argument"}},
		{Args: []string{"env"}, Env: map[string]string{"": "missing-key"}},
		{Args: []string{"env"}, Env: map[string]string{"BAD=KEY": "value"}},
		{Args: []string{"env"}, Env: map[string]string{"BAD\x00KEY": "value"}},
		{Args: []string{"env"}, Env: map[string]string{"KEY": "bad\x00value"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted invalid command %#v", invalid)
		}
	}
}

func TestSameGuestTreatsLegacyEmptyBackendAsCNI(t *testing.T) {
	legacy := SandboxConfig{Name: "demo", CPUs: 1, Memory: MinSandboxMemory, Storage: MinSandboxStorage, NICs: 1, NetworkName: "default"}
	explicit := legacy
	explicit.NetworkBackend = NetworkBackendCNI
	if !legacy.SameGuest(explicit) {
		t.Fatal("legacy empty backend is not compatible with explicit CNI")
	}
	explicit.NetworkBackend = NetworkBackendBridge
	explicit.NetworkName = "br0"
	if legacy.SameGuest(explicit) {
		t.Fatal("CNI and bridge configs compared equal")
	}
}
