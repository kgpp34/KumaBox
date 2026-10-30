package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestLoaderPrecedenceAndIsolation(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "config.yaml")
	contents := []byte("paths:\n  data: " + filepath.Join(base, "file-data") + "\nimages:\n  parallelism: 2\nvmm:\n  cloud_hypervisor:\n    startup_timeout: 12s\n")
	if err := os.WriteFile(file, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUMABOX_IMAGES_PARALLELISM", "3")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("root-dir", "", "")
	if err := flags.Set("root-dir", filepath.Join(base, "flag-data")); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader()
	if err := loader.BindFlag("paths.data", flags.Lookup("root-dir")); err != nil {
		t.Fatal(err)
	}
	got, err := loader.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	expected := Default()
	expected.Paths.Data = filepath.Join(base, "flag-data")
	if err := expected.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Paths.Data != expected.Paths.Data || got.Images.Parallelism != 3 || got.VMM.CloudHypervisor.StartupTimeout != 12*time.Second {
		t.Fatalf("resolved config = %+v", got)
	}
	expected = Default()
	if err := expected.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Paths.Run != expected.Paths.Run || got.Sandbox.Ext4Binary != "mkfs.ext4" {
		t.Fatalf("defaults were not retained: %+v", got)
	}

	isolated, err := NewLoader().Load("")
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Paths.Data != expected.Paths.Data {
		t.Fatalf("loader state leaked: data root = %q", isolated.Paths.Data)
	}
}

func TestUnchangedFlagDoesNotOverrideFile(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "config.json")
	dataRoot := filepath.Join(base, "file-data")
	if err := os.WriteFile(file, []byte(`{"paths":{"data":"`+dataRoot+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("root-dir", Default().Paths.Data, "")
	loader := NewLoader()
	if err := loader.BindFlag("paths.data", flags.Lookup("root-dir")); err != nil {
		t.Fatal(err)
	}
	got, err := loader.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	expected := Default()
	expected.Paths.Data = dataRoot
	if err := expected.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Paths.Data != expected.Paths.Data {
		t.Fatalf("data root = %q, want file value %q", got.Paths.Data, expected.Paths.Data)
	}
}

func TestLoaderRejectsInvalidInput(t *testing.T) {
	base := t.TempDir()
	for _, test := range []struct {
		name    string
		path    string
		content string
	}{
		{name: "missing explicit file", path: filepath.Join(base, "missing.yaml")},
		{name: "unknown key", path: filepath.Join(base, "unknown.yaml"), content: "unknown: true\n"},
		{name: "invalid duration", path: filepath.Join(base, "duration.yaml"), content: "metadata:\n  retry_limit: soon\n"},
		{name: "invalid limit", path: filepath.Join(base, "limit.yaml"), content: "images:\n  parallelism: 0\n"},
		{name: "negative access interval", path: filepath.Join(base, "access.yaml"), content: "snapshots:\n  access_flush_interval: -1ms\n"},
		{name: "unbounded access buffer", path: filepath.Join(base, "buffer.yaml"), content: "snapshots:\n  access_buffer_size: 0\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.content != "" {
				if err := os.WriteFile(test.path, []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewLoader().Load(test.path); err == nil {
				t.Fatal("Load() accepted invalid configuration")
			}
		})
	}
}

func TestValidateRejectsOverlappingRoots(t *testing.T) {
	config := Default()
	config.Paths.Data = t.TempDir()
	config.Paths.Run = filepath.Join(config.Paths.Data, "run")
	config.Paths.Log = filepath.Join(t.TempDir(), "log")
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted overlapping roots")
	}
}

func TestNetworkConfigParsesDNSAndScope(t *testing.T) {
	config := Default()
	config.Network.DNS = "10.0.0.2; 2001:4860:4860::8888"
	config.Network.Scope = "k1"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	servers, err := config.Network.DNSServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0] != "10.0.0.2" || config.Network.NamespacePrefix() != "k1-" {
		t.Fatalf("servers=%v prefix=%q", servers, config.Network.NamespacePrefix())
	}
	config.Network.Scope = "unsafe/"
	if err := config.Validate(); err == nil {
		t.Fatal("invalid network scope was accepted")
	}
	config = Default()
	config.Network.DNS = "not-an-address"
	if err := config.Validate(); err == nil {
		t.Fatal("invalid DNS server was accepted")
	}
}

func TestTAPPoolConfigurationRejectsUnboundedAndIncompatibleShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		queues []int
	}{
		{"negative", -1, nil},
		{"too large", 65, nil},
		{"odd queues", 4, []int{3}},
		{"duplicate queues", 4, []int{2, 2}},
		{"zero queues", 4, []int{0}},
		{"too many queues", 4, []int{258}},
		{"idle device budget", 64, []int{2, 4, 6, 8, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Network.EBPF.TAPPoolSize, cfg.Network.EBPF.TAPPoolQueues = tc.size, tc.queues
			if err := cfg.Validate(); err == nil {
				t.Fatal("accepted invalid TAP pool configuration")
			}
		})
	}
}

func TestStopTimeoutConfiguration(t *testing.T) {
	if got := Default().Sandbox.StopTimeout; got != 30*time.Second {
		t.Fatalf("default stop timeout = %s", got)
	}
	t.Setenv("KUMABOX_SANDBOX_STOP_TIMEOUT", "1500ms")
	configuration, err := NewLoader().Load("")
	if err != nil || configuration.Sandbox.StopTimeout != 1500*time.Millisecond {
		t.Fatalf("configured stop timeout = %s, %v", configuration.Sandbox.StopTimeout, err)
	}
	for _, value := range []time.Duration{0, -time.Second} {
		configuration.Sandbox.StopTimeout = value
		if err := configuration.Validate(); err == nil {
			t.Fatalf("accepted stop timeout %s", value)
		}
	}
}

func TestMachineIDSyncDefaultsAndOverride(t *testing.T) {
	if !Default().Sandbox.MachineIDSync {
		t.Fatal("default no longer waits for identity persistence")
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("sandbox:\n  machine_id_sync: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := NewLoader().Load(file)
	if err != nil || configuration.Sandbox.MachineIDSync {
		t.Fatalf("writeback policy = %v, %v", configuration.Sandbox.MachineIDSync, err)
	}
	t.Setenv("KUMABOX_SANDBOX_MACHINE_ID_SYNC", "true")
	configuration, err = NewLoader().Load(file)
	if err != nil || !configuration.Sandbox.MachineIDSync {
		t.Fatalf("environment policy = %v, %v", configuration.Sandbox.MachineIDSync, err)
	}
}
