package cloudhypervisor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInspectRestoreModesRequiresBinarySchemaMarkers(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{name: "copy only", content: "cloud-hypervisor", want: []string{"copy"}},
		{name: "all modes", content: "memory_restore_mode OnDemand memory_restore_mode=copy|ondemand|mmap", want: []string{"copy", "ondemand", "mmap"}},
		{name: "unrelated mmap marker", content: "memory_restore_mode OnDemand InvalidDeviceExcludeMmapBar", want: []string{"copy", "ondemand"}},
		{name: "enum without field", content: "OnDemand Mmap", want: []string{"copy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cloud-hypervisor")
			if err := os.WriteFile(path, []byte(tt.content), 0o700); err != nil {
				t.Fatal(err)
			}
			if got := inspectRestoreModes(path); !slices.Equal(got, tt.want) {
				t.Fatalf("restore modes = %v, want %v", got, tt.want)
			}
		})
	}
}
