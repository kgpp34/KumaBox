package types

import "testing"

func TestGuestPathValidation(t *testing.T) {
	for _, test := range []struct {
		path               string
		destructive, valid bool
	}{
		{"/", false, true},
		{"/", true, false},
		{"/tmp/data", true, true},
		{"relative", false, false},
		{"/tmp/../etc", false, false},
		{"/tmp/", false, false},
		{"/tmp//data", false, false},
		{"/tmp/\x00", false, false},
		{"/tmp/\n", false, false},
	} {
		if err := ValidateGuestPath(test.path, test.destructive); (err == nil) != test.valid {
			t.Errorf("path %q destructive=%v: %v", test.path, test.destructive, err)
		}
	}
}

func TestFileWriteModeValidation(t *testing.T) {
	for _, mode := range []uint32{0, 0o600, 0o777, 0o1000, 0o100644} {
		if err := (FileWriteOptions{Mode: &mode}).Validate(); (err == nil) != (mode <= 0o777) {
			t.Errorf("mode %o: %v", mode, err)
		}
	}
}
