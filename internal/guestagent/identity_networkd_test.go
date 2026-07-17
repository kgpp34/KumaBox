package guestagent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPersistNetworkdIdentity(t *testing.T) {
	originalDir := networkdConfigDir
	networkdConfigDir = t.TempDir()
	t.Cleanup(func() { networkdConfigDir = originalDir })

	stale := filepath.Join(networkdConfigDir, networkdIdentityPrefix+"stale.network")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	identities := []interfaceIdentity{{
		Name: "eth0", MAC: "FA:49:C4:3E:C0:85", IP: "10.88.0.3", Prefix: 16,
		Gateway: "10.88.0.1", DNS: []string{"1.1.1.1", "8.8.8.8"},
	}}

	names, err := persistNetworkdIdentity(identities)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"eth0"}) {
		t.Fatalf("interface names = %v", names)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale identity remains: %v", err)
	}
	path := filepath.Join(networkdConfigDir, networkdIdentityPrefix+"fa49c43ec085.network")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"MACAddress=fa:49:c4:3e:c0:85", "DHCP=no", "Address=10.88.0.3/16",
		"Gateway=10.88.0.1", "DNS=1.1.1.1", "DNS=8.8.8.8",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("network config %q does not contain %q", content, want)
		}
	}
}

func TestRenderNetworkdIdentityRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		identity interfaceIdentity
	}{
		{name: "MAC", identity: interfaceIdentity{Name: "eth0", MAC: "bad", IP: "10.88.0.3", Prefix: 16}},
		{name: "name", identity: interfaceIdentity{Name: "", MAC: "02:00:00:00:00:01", IP: "10.88.0.3", Prefix: 16}},
		{name: "IP", identity: interfaceIdentity{Name: "eth0", MAC: "02:00:00:00:00:01", IP: "bad", Prefix: 16}},
		{name: "prefix", identity: interfaceIdentity{Name: "eth0", MAC: "02:00:00:00:00:01", IP: "10.88.0.3", Prefix: 33}},
		{name: "gateway", identity: interfaceIdentity{Name: "eth0", MAC: "02:00:00:00:00:01", IP: "10.88.0.3", Prefix: 16, Gateway: "bad"}},
		{name: "DNS", identity: interfaceIdentity{Name: "eth0", MAC: "02:00:00:00:00:01", IP: "10.88.0.3", Prefix: 16, DNS: []string{"bad"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := renderNetworkdIdentity(tt.identity); err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}
}

func TestReconfigureNetworkdUsesStableInterfaceOrder(t *testing.T) {
	originalStateDir := networkdStateDir
	originalRun := runNetworkctl
	networkdStateDir = t.TempDir()
	var calls [][]string
	runNetworkctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() {
		networkdStateDir = originalStateDir
		runNetworkctl = originalRun
	})

	if err := reloadNetworkd(); err != nil {
		t.Fatal(err)
	}
	if err := reconfigureNetworkd([]string{"eth1", "eth0"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"reload"}, {"reconfigure", "eth0"}, {"reconfigure", "eth1"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("networkctl calls = %v, want %v", calls, want)
	}
}
