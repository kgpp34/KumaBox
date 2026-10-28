package daemon

import (
	"context"
	"errors"
	"net/http"
	"os/exec"

	"github.com/kumabox/kumabox/rpc"
)

// registerDoctor runs only the installed prerequisite checker. Requests cannot
// choose another host executable; socket permissions grant administrative access.
func registerDoctor(mux *http.ServeMux) {
	rpc.Register(mux, "node.doctor", func(ctx context.Context, s *rpc.Session, args []string) (any, error) {
		check := exec.CommandContext(ctx, "kumabox-check", args...) //nolint:gosec // root-only socket selects arguments for this fixed installed checker
		check.Stdin, check.Stdout, check.Stderr = s.Input, s.Output("stdout"), s.Output("stderr")
		if err := check.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return exit.ExitCode(), nil
			}
			return nil, err
		}
		return 0, nil
	})
}
