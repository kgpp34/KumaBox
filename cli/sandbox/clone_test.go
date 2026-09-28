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
	err := command.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("clone error = %v", err)
	}
	if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("clone error code = %q, %v", code, ok)
	}
}
