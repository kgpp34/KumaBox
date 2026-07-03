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
