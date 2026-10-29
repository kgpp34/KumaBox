package agent

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testNetworkConfig() NetworkConfig {
	return NetworkConfig{
		Hostname: "clone-box", DNSServers: []string{"1.1.1.1"},
		Interfaces: []NetworkInterface{{MAC: "02:00:00:00:00:02", Address: "10.0.0.3", Prefix: 24, Gateway: "10.0.0.1"}},
	}
}

func TestConfigureNetworkExchangesStructuredMessage(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()  //nolint:errcheck // test cleanup
	defer guest.Close() //nolint:errcheck // test cleanup
	done := make(chan error, 1)
	go func() {
		message, err := NewDecoder(guest).Decode()
		if err != nil {
			done <- err
			return
		}
		if message.Type != MessageConfigureNetwork || message.Network == nil || message.Network.Hostname != "clone-box" {
			done <- errors.New("network request was not structured")
			return
		}
		done <- NewEncoder(guest).Encode(Message{Type: MessageExit})
	}()
	if err := ConfigureNetwork(t.Context(), host, testNetworkConfig()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfigureNetworkIdentifiesOldAgent(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()  //nolint:errcheck // test cleanup
	defer guest.Close() //nolint:errcheck // test cleanup
	go func() {
		_, _ = NewDecoder(guest).Decode()
		_ = NewEncoder(guest).Encode(Message{Type: MessageError, Message: `expected first frame type "exec", got "configure_network"`})
	}()
	if err := ConfigureNetwork(t.Context(), host, testNetworkConfig()); !errors.Is(err, ErrNetworkConfigUnsupported) {
		t.Fatalf("old guest response = %v", err)
	}
}

func TestWriteNetworkFilesReplacesOnlyManagedFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "other.network"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := testNetworkConfig()
	if err := writeNetworkFiles(directory, old); err != nil {
		t.Fatal(err)
	}
	newConfig := testNetworkConfig()
	newConfig.Interfaces[0].MAC = "02:00:00:00:00:03"
	if err := writeNetworkFiles(directory, newConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "10-kumabox-020000000002.network")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old network file remains: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "10-kumabox-020000000003.network"))
	if err != nil || !strings.Contains(string(content), "Address=10.0.0.3/24") {
		t.Fatalf("new network file = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "other.network")); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkConfigRejectsUnsafeValues(t *testing.T) {
	for _, mutate := range []func(*NetworkConfig){
		func(config *NetworkConfig) { config.Hostname = "bad'host" },
		func(config *NetworkConfig) { config.Interfaces[0].MAC = "../../bad" },
		func(config *NetworkConfig) { config.Interfaces[0].Address = "invalid" },
		func(config *NetworkConfig) { config.DNSServers = []string{"invalid"} },
		func(config *NetworkConfig) {
			config.Interfaces[0].DHCP = true
		},
	} {
		config := testNetworkConfig()
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted invalid configuration %+v", config)
		}
	}
}

func TestNetworkFilesMatchOnlyAnUnchangedStaticConfiguration(t *testing.T) {
	directory := t.TempDir()
	configuration := testNetworkConfig()
	configuration.Interfaces[0].Address = "169.254.240.2"
	configuration.Interfaces[0].Prefix = 30
	configuration.Interfaces[0].Gateway = "169.254.240.1"
	if err := writeNetworkFiles(directory, configuration); err != nil {
		t.Fatal(err)
	}
	if !networkFilesMatch(directory, configuration) {
		t.Fatal("fixed guest configuration did not match")
	}
	file, err := os.ReadFile(filepath.Join(directory, "10-kumabox-020000000002.network"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file), "Scope=global") || !strings.Contains(string(file), "PreferredSource=169.254.240.2") {
		t.Fatal("fixed network lost explicit address/route scope")
	}
	configuration.Hostname = "another-clone"
	if !networkFilesMatch(directory, configuration) {
		t.Fatal("hostname change invalidated the network configuration")
	}
	configuration.DNSServers = []string{"8.8.8.8"}
	if networkFilesMatch(directory, configuration) {
		t.Fatal("changed resolver was ignored")
	}
	configuration.DNSServers = []string{"1.1.1.1"}
	if err := os.WriteFile(filepath.Join(directory, "10-kumabox-stale.network"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if networkFilesMatch(directory, configuration) {
		t.Fatal("stale managed file was ignored")
	}
}

func TestFixedResolverUsesDirectServersAndRemainsStable(t *testing.T) {
	root := t.TempDir()
	configuration := testNetworkConfig()
	configuration.Interfaces[0].Address = "169.254.240.2"
	configuration.Interfaces[0].Gateway = "169.254.240.1"
	if err := writeFixedResolverAt(root, configuration); err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(root, "etc/resolv.conf")
	target, err := os.Readlink(resolver)
	if err != nil || target != "/etc/kumabox/resolv.conf" {
		t.Fatalf("resolver link: %s %v", target, err)
	}
	path := filepath.Join(root, "etc/kumabox/resolv.conf")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFixedResolverAt(root, configuration); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged resolver was republished")
	}
	configuration.DNSServers = []string{"8.8.8.8"}
	if err := writeFixedResolverAt(root, configuration); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "nameserver 8.8.8.8\n" {
		t.Fatalf("updated resolver: %s %v", data, err)
	}
}

func TestNetworkConfigAcceptsDHCPAndWritesCloneSafeUnit(t *testing.T) {
	configuration := testNetworkConfig()
	configuration.Interfaces[0].DHCP = true
	configuration.Interfaces[0].Address = ""
	configuration.Interfaces[0].Prefix = 0
	configuration.Interfaces[0].Gateway = ""
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := writeNetworkFiles(directory, configuration); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "10-kumabox-020000000002.network"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DHCP=ipv4", "ClientIdentifier=mac"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("DHCP network file %q does not contain %q", content, want)
		}
	}
}

func TestNetworkConfigAcceptsIPv6DNS(t *testing.T) {
	configuration := testNetworkConfig()
	configuration.DNSServers = []string{"2001:4860:4860::8888"}
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResolverClearsCapturedServers(t *testing.T) {
	root := t.TempDir()
	if err := writeResolverAt(root, []string{"1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if err := writeResolverAt(root, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/kumabox/resolv.conf"))
	if err != nil || len(data) != 0 {
		t.Fatalf("stale DNS remained after clearing: %q %v", data, err)
	}
}
