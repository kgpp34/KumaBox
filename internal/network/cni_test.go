package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
)

func TestAddCNICallsPluginAndParsesResult(t *testing.T) {
	dir := t.TempDir()
	cfg, logPath := writeTestCNIConfig(t, dir, false)
	withCNIDatapath(t)

	allocation, err := AddCNI(context.Background(), dir, cfg, CNIAddRequest{
		VMID:    "kb_cni",
		Network: "cni:default",
		Index:   0,
		CPU:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Record.Provider != ProviderCNI {
		t.Fatalf("provider = %s", allocation.Record.Provider)
	}
	if allocation.Record.Network != "cni:default" {
		t.Fatalf("network = %s", allocation.Record.Network)
	}
	if allocation.Record.TAP == "" || allocation.Record.TAP != allocation.Config.TAP {
		t.Fatalf("tap mismatch: record=%s config=%s", allocation.Record.TAP, allocation.Config.TAP)
	}
	if allocation.Record.IfName != "eth0" || allocation.Config.IfName != "eth0" {
		t.Fatalf("ifname mismatch: record=%s config=%s", allocation.Record.IfName, allocation.Config.IfName)
	}
	if allocation.Record.NetnsPath != NetNSPath("kb_cni") || allocation.Config.NetnsPath != NetNSPath("kb_cni") {
		t.Fatalf("netns mismatch: record=%s config=%s", allocation.Record.NetnsPath, allocation.Config.NetnsPath)
	}
	if allocation.Config.Backend != ProviderCNI {
		t.Fatalf("config backend = %s", allocation.Config.Backend)
	}
	if allocation.Config.Network == nil || allocation.Config.Network.IP != "10.244.0.2" {
		t.Fatalf("guest network = %+v", allocation.Config.Network)
	}
	if allocation.Config.Network.Gateway != "10.244.0.1" || allocation.Config.Network.Prefix != 24 {
		t.Fatalf("guest network = %+v", allocation.Config.Network)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ADD kb_cni eth0 "+NetNSPath("kb_cni")) {
		t.Fatalf("plugin log = %s", raw)
	}
}

func TestDeleteCNICallsPlugin(t *testing.T) {
	dir := t.TempDir()
	cfg, logPath := writeTestCNIConfig(t, dir, false)
	withCNIDatapath(t)

	if err := DeleteCNI(context.Background(), dir, cfg, CNIDeleteRequest{
		VMID:      "kb_cni",
		Network:   "cni:default",
		IfName:    "eth0",
		TAP:       "kbtapcni0",
		NetNSPath: NetNSPath("kb_cni"),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "DEL kb_cni eth0 "+NetNSPath("kb_cni")) {
		t.Fatalf("plugin log = %s", raw)
	}
}

func TestDeleteCNIReportsPluginFailure(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := writeTestCNIConfig(t, dir, true)
	withCNIDatapath(t)

	err := DeleteCNI(context.Background(), dir, cfg, CNIDeleteRequest{
		VMID:      "kb_cni",
		Network:   "cni:default",
		IfName:    "eth0",
		TAP:       "kbtapcni0",
		NetNSPath: NetNSPath("kb_cni"),
	})
	if err == nil || !strings.Contains(err.Error(), "forced del failure") {
		t.Fatalf("delete error = %v", err)
	}
}

func withCNIDatapath(t *testing.T) {
	t.Helper()
	oldPrepare := prepareCNINetns
	oldSetup := setupCNIDatapath
	oldDeleteDatapath := deleteCNIDatapath
	oldDeleteNetns := deleteCNINetns
	prepareCNINetns = func(vmID, requestedPath string) (string, bool, error) {
		if requestedPath != "" {
			return requestedPath, false, nil
		}
		return NetNSPath(vmID), true, nil
	}
	setupCNIDatapath = func(_ string, _ string, _ string, _ int, mac string) (string, error) {
		return mac, nil
	}
	deleteCNIDatapath = func(_, _ string) error {
		return nil
	}
	deleteCNINetns = func(_, _ string) error {
		return nil
	}
	t.Cleanup(func() {
		prepareCNINetns = oldPrepare
		setupCNIDatapath = oldSetup
		deleteCNIDatapath = oldDeleteDatapath
		deleteCNINetns = oldDeleteNetns
	})
}

func TestAddCNIFailsWhenConfigMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default().Network
	cfg.CNIConfigDir = filepath.Join(dir, "missing")
	cfg.CNIBinDir = filepath.Join(dir, "bin")

	_, err := AddCNI(context.Background(), dir, cfg, CNIAddRequest{
		VMID:    "kb_cni",
		Network: "cni:missing",
	})
	if err == nil || !strings.Contains(err.Error(), "load cni config") {
		t.Fatalf("add error = %v", err)
	}
}

func writeTestCNIConfig(t *testing.T, dir string, failDel bool) (config.NetworkConfig, string) {
	t.Helper()
	cfg := config.Default().Network
	cfg.CNIConfigDir = filepath.Join(dir, "net.d")
	cfg.CNIBinDir = filepath.Join(dir, "bin")
	if err := os.MkdirAll(cfg.CNIConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.CNIBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CNIConfigDir, "default.conf"), []byte(`{
  "cniVersion": "1.0.0",
  "name": "default",
  "type": "kumabox-test"
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "cni.log")
	delFailure := ""
	if failDel {
		delFailure = "echo forced del failure >&2\nexit 7"
	}
	plugin := `#!/bin/sh
set -eu
cat >/dev/null
printf '%s %s %s %s\n' "$CNI_COMMAND" "$CNI_CONTAINERID" "$CNI_IFNAME" "$CNI_NETNS" >> "` + logPath + `"
if [ "$CNI_COMMAND" = "ADD" ]; then
  printf '{"cniVersion":"1.0.0","interfaces":[{"name":"%s","mac":"5a:00:00:00:00:44","sandbox":"%s"}],"ips":[{"address":"10.244.0.2/24","gateway":"10.244.0.1","interface":0}],"dns":{"nameservers":["1.1.1.1"]}}\n' "$CNI_IFNAME" "$CNI_NETNS"
  exit 0
fi
if [ "$CNI_COMMAND" = "DEL" ]; then
` + delFailure + `
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(cfg.CNIBinDir, "kumabox-test"), []byte(plugin), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, logPath
}
