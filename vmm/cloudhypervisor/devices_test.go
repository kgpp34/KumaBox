package cloudhypervisor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureRejectsRuntimeExternalDevices(t *testing.T) {
	var info vmInfo
	if err := json.Unmarshal([]byte(`{"state":"Running","config":{"disks":[{"id":"kumabox-disk-volume","path":"/srv/volume.raw","serial":"volume"}]}}`), &info); err != nil {
		t.Fatal(err)
	}
	if err := refuseExternalDevices(info); err == nil || !strings.Contains(err.Error(), "volume") {
		t.Fatalf("external disk guard = %v", err)
	}
	if err := json.Unmarshal([]byte(`{"state":"Running","config":{"devices":[{"id":"gpu","path":"/sys/bus/pci/devices/0000:01:00.0"}]}}`), &info); err != nil {
		t.Fatal(err)
	}
	info.Config.Disks = nil
	if err := refuseExternalDevices(info); err == nil {
		t.Fatal("VFIO passthrough guard did not reject capture")
	}
	info.Config.Devices = nil
	if err := refuseExternalDevices(info); err != nil {
		t.Fatalf("plain sandbox cannot be captured: %v", err)
	}
}
