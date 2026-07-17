//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWritableBlockMountsSelectsWritableDeviceMounts(t *testing.T) {
	t.Parallel()

	mountInfo := `36 25 0:32 / / rw,relatime - overlay overlay rw,lowerdir=/layers,upperdir=/cow/upper
37 36 253:6 /control /.kumabox/cow rw,relatime - ext4 /dev/vdg rw
38 36 253:0 / /boot ro,relatime - ext4 /dev/vda ro
39 36 0:40 / /run rw,nosuid,nodev - tmpfs tmpfs rw
40 36 253:7 /data /var/lib/application\040data rw,relatime - xfs /dev/vdh rw
`
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(mountInfo), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := writableBlockMounts(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/var/lib/application data", "/.kumabox/cow"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("writableBlockMounts() = %q, want %q", got, want)
	}
}
