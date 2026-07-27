package metadata

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderNoCloudFiles(t *testing.T) {
	rendered, err := Render(Config{
		InstanceID: "kb_test",
		Hostname:   "p1-meta",
	})
	if err != nil {
		t.Fatal(err)
	}

	metaData := string(rendered.MetaData)
	if !strings.Contains(metaData, "instance-id: kb_test") {
		t.Fatalf("meta-data = %s", metaData)
	}
	if !strings.Contains(metaData, "local-hostname: p1-meta") {
		t.Fatalf("meta-data = %s", metaData)
	}

	userData := string(rendered.UserData)
	if !strings.Contains(userData, "#cloud-config") {
		t.Fatalf("user-data = %s", userData)
	}
	if !strings.Contains(userData, "name: kumabox") {
		t.Fatalf("user-data = %s", userData)
	}

	networkConfig := string(rendered.NetworkConfig)
	if !strings.Contains(networkConfig, "dhcp4: true") {
		t.Fatalf("network-config = %s", networkConfig)
	}
}

func TestRenderStaticNetworkConfig(t *testing.T) {
	rendered, err := Render(Config{
		InstanceID: "kb_test",
		Hostname:   "p2-net",
		Networks: []Network{{
			MAC:     "02:00:00:00:00:11",
			IP:      "10.88.0.2",
			Prefix:  16,
			Gateway: "10.88.0.1",
			DNS:     []string{"1.1.1.1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	networkConfig := string(rendered.NetworkConfig)
	for _, want := range []string{
		`macaddress: "02:00:00:00:00:11"`,
		"set-name: eth0",
		"10.88.0.2/16",
		"gateway4: 10.88.0.1",
		"1.1.1.1",
	} {
		if !strings.Contains(networkConfig, want) {
			t.Fatalf("network-config missing %q:\n%s", want, networkConfig)
		}
	}
	if strings.Contains(networkConfig, "dhcp4: true") {
		t.Fatalf("static network-config should not include DHCP fallback:\n%s", networkConfig)
	}
}

func TestRenderManagedDataDiskMount(t *testing.T) {
	rendered, err := Render(Config{
		InstanceID: "kb_test", Hostname: "data", Mounts: []Mount{{Device: "/dev/disk/by-id/virtio-workspace", MountPoint: "/mnt/workspace", Filesystem: "ext4", Options: "defaults,nofail"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	userData := string(rendered.UserData)
	for _, want := range []string{"mounts:", "/dev/disk/by-id/virtio-workspace", "/mnt/workspace", "defaults,nofail"} {
		if !strings.Contains(userData, want) {
			t.Fatalf("user-data missing %q:\n%s", want, userData)
		}
	}
}

func TestWriteNoCloudImage(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNoCloudImage(&buf, Config{
		InstanceID: "kb_test",
		Hostname:   "p1-meta",
	}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != fatSectorSize*fatTotalSectors {
		t.Fatalf("image size = %d", buf.Len())
	}
	if !bytes.Contains(buf.Bytes()[43:54], []byte("CIDATA")) {
		t.Fatalf("CIDATA label not found in boot sector")
	}
	if !ContainsNoCloudFiles(buf.Bytes()) {
		t.Fatalf("NoCloud files not found in FAT image")
	}
}

func TestWriteNoCloudWritesSourceFilesAndDisk(t *testing.T) {
	dir := t.TempDir()
	cidataDir := filepath.Join(dir, "cidata")
	cidataDisk := filepath.Join(dir, "cidata.img")

	if err := WriteNoCloud(cidataDir, cidataDisk, Config{
		InstanceID: "kb_test",
		Hostname:   "p1-meta",
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"meta-data", "user-data", "network-config"} {
		if _, err := os.Stat(filepath.Join(cidataDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	info, err := os.Stat(cidataDisk)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != fatSectorSize*fatTotalSectors {
		t.Fatalf("cidata size = %d", info.Size())
	}
}
