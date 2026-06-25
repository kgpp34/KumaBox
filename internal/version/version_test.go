package version

import "testing"

func TestInfo(t *testing.T) {
	info := Info()
	if info.Version == "" {
		t.Fatal("version must not be empty")
	}
	if info.Commit == "" {
		t.Fatal("commit must not be empty")
	}
	if info.BuildTime == "" {
		t.Fatal("build time must not be empty")
	}
}
