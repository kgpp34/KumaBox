package cloudhypervisor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/vmm"
)

func TestNewUsesConfiguredLifecyclePolicy(t *testing.T) {
	base := t.TempDir()
	paths, err := vmm.NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := cgroup.New("/sys/fs/cgroup/kumabox-test.slice")
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Binary: "custom-vmm", StartupTimeout: 2 * time.Second,
		StopGrace: 3 * time.Second, AbortGrace: 4 * time.Second,
	}
	driver, err := New(paths, scopes, options)
	if err != nil {
		t.Fatal(err)
	}
	if driver.binary != options.Binary || driver.startupTimeout != options.StartupTimeout ||
		driver.stopGrace != options.StopGrace || driver.abortGrace != options.AbortGrace {
		t.Fatalf("driver policy = %+v", driver)
	}
}

func TestNewRejectsInvalidLifecyclePolicy(t *testing.T) {
	scopes, err := cgroup.New("/sys/fs/cgroup/kumabox-test.slice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(vmm.Paths{}, scopes, Options{StartupTimeout: time.Nanosecond}); err == nil {
		t.Fatal("New() accepted a startup timeout shorter than one probe")
	}
}
