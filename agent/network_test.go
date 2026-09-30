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
	} {
		config := testNetworkConfig()
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted invalid configuration %+v", config)
		}
	}
}
