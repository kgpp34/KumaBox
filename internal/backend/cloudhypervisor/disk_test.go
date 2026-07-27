package cloudhypervisor

import "testing"

func TestValidDiskName(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"workspace", true}, {"data_1", true}, {"1data", false}, {"bad.name", false}, {"", false},
	} {
		if got := validDiskName(test.name); got != test.valid {
			t.Errorf("validDiskName(%q) = %v, want %v", test.name, got, test.valid)
		}
	}
}
