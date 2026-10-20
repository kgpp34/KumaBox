package sandbox

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestFSAttachValidatesRequestBeforeOpeningSandbox(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "relative socket", args: []string{"attach", "box", "--socket", "relative.sock", "--tag", "data"}},
		{name: "bad tag", args: []string{"attach", "box", "--socket", "/run/virtiofsd.sock", "--tag", "bad/tag"}},
		{name: "negative queues", args: []string{"attach", "box", "--socket", "/run/virtiofsd.sock", "--tag", "data", "--num-queues", "-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := NewFSCommand(func() config.Config {
				t.Fatal("invalid fs attachment opened the sandbox service")
				return config.Config{}
			})
			command.SetArgs(test.args)
			command.SilenceUsage, command.SilenceErrors = true, true
			err := command.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), "file share") {
				t.Fatalf("fs attach error = %v", err)
			}
			if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
				t.Fatalf("fs attach error code = %q, %v", code, ok)
			}
		})
	}
}
