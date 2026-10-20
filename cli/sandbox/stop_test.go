package sandbox

import (
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestStopRejectsInvalidTimeoutBeforeOpeningService(t *testing.T) {
	for _, timeout := range []string{"-1", "9223372036854775807"} {
		t.Run(timeout, func(t *testing.T) {
			command := NewStopCommand(func() config.Config {
				t.Fatal("invalid timeout opened the service")
				return config.Config{}
			})
			command.SilenceErrors, command.SilenceUsage = true, true
			command.SetArgs([]string{"box", "--timeout", timeout})
			err := command.ExecuteContext(t.Context())
			if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
				t.Fatalf("stop error = %v", err)
			}
		})
	}
}
