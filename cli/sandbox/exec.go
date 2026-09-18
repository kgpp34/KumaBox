package sandbox

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// commandExitError propagates a guest command's status without printing an
// extra KumaBox diagnostic after the guest has already written its output.
type commandExitError struct {
	code int
}

func (e *commandExitError) Error() string {
	return fmt.Sprintf("guest command exited with status %d", e.code)
}
func (e *commandExitError) ExitCode() int { return e.code }
func (e *commandExitError) Silent() bool  { return true }

// NewExecCommand builds the streaming guest exec command.
func NewExecCommand(configuration configProvider) *cobra.Command {
	var environment []string
	var interactive bool
	command := &cobra.Command{
		Use:   "exec [flags] SANDBOX -- COMMAND [ARGS...]",
		Short: "run a command inside a running sandbox",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			environmentMap, err := parseEnvironment(environment)
			if err != nil {
				return invalidFlag("env", err)
			}
			invocation := types.Command{Args: append([]string(nil), args[1:]...), Env: environmentMap}
			if err := invocation.Validate(); err != nil {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "execute sandbox command", args[0], "close metadata", "retry the command", false))
			}()
			var input io.Reader
			if interactive {
				input = command.InOrStdin()
			}
			exitCode, err := service.Exec(command.Context(), args[0], invocation, input, command.OutOrStdout(), command.ErrOrStderr())
			if err != nil {
				return err
			}
			if exitCode != 0 {
				return &commandExitError{code: exitCode}
			}
			return nil
		},
	}
	command.Flags().StringArrayVarP(&environment, "env", "e", nil, "set a guest environment variable in KEY=VALUE form (repeatable)")
	command.Flags().BoolVarP(&interactive, "interactive", "i", false, "attach stdin to the guest command")
	return command
}

// parseEnvironment converts repeatable CLI values at the presentation
// boundary. Later occurrences replace earlier ones, matching common CLI flag
// behavior without leaking KEY=VALUE syntax into core or the guest client.
func parseEnvironment(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%q must be KEY=VALUE", entry)
		}
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, fmt.Errorf("%q must not contain NUL bytes", entry)
		}
		environment[key] = value
	}
	return environment, nil
}
