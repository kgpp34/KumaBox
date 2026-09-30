package sandbox

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestCloneRequiresNameBeforeOpeningService(t *testing.T) {
	command := NewCloneCommand(func() config.Config {
		t.Fatal("clone opened service without a target name")
		return config.Config{}
	})
	command.SetArgs([]string{"checkpoint"})
	command.SilenceUsage = true
	command.SilenceErrors = true
	err := command.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("clone error = %v", err)
	}
	if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("clone error code = %q, %v", code, ok)
	}
}

func TestCloneValidatesNewDataDiskBeforeOpeningService(t *testing.T) {
	command := NewCloneCommand(func() config.Config {
		t.Fatal("invalid clone data disk opened service")
		return config.Config{}
	})
	command.SetArgs([]string{"checkpoint", "--name", "copy", "--data-disk", "size=16MiB,directio=maybe"})
	command.SilenceUsage, command.SilenceErrors = true, true
	if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "--data-disk") {
		t.Fatalf("clone data disk error = %v", err)
	}
}
