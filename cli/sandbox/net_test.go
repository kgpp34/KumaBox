package sandbox

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestNetRejectsMissingOrOutOfRangeTargetBeforeOpeningService(t *testing.T) {
	for _, args := range [][]string{
		{"box"},
		{"box", "--nics=-1"},
		{"box", "--nics=65"},
	} {
		command := NewNetCommand(func() config.Config {
			t.Fatal("net opened service for invalid NIC target")
			return config.Config{}
		})
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		if err == nil || !strings.Contains(err.Error(), "--nics") {
			t.Fatalf("net %v error = %v", args, err)
		}
		if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
			t.Fatalf("net %v code = %q, %v", args, code, ok)
		}
	}
}
