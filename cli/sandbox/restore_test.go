package sandbox

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestRestoreRejectsAmbiguousSourceBeforeOpeningService(t *testing.T) {
	for _, args := range [][]string{{"box"}, {"box", "capture", "--from-dir", "/tmp/capture"}, {"box", "capture", "--force"}} {
		command := NewRestoreCommand(func() config.Config {
			t.Fatal("restore opened service for invalid source flags")
			return config.Config{}
		})
		command.SetArgs(args)
		command.SilenceUsage = true
		command.SilenceErrors = true
		err := command.ExecuteContext(t.Context())
		if err == nil || (!strings.Contains(err.Error(), "--from-dir") && !strings.Contains(err.Error(), "--force")) {
			t.Fatalf("restore %v error = %v", args, err)
		}
		if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
			t.Fatalf("restore %v error code = %q, %v", args, code, ok)
		}
	}
}
