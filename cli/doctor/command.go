// Package doctor submits host prerequisite checks to the node service.
package doctor

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/client"
	"github.com/kumabox/kumabox/config"
)

// checkerName is the separately installed bootstrap and diagnostic script.
const checkerName = "kumabox-check"

type processError struct{ code int }

func (e *processError) Error() string {
	return fmt.Sprintf("host checker exited with status %d", e.code)
}
func (e *processError) ExitCode() int { return e.code }
func (e *processError) Silent() bool  { return true }

// NewCommand forwards checker flags unchanged through the local node API.
func NewCommand(configuration func() config.Config) *cobra.Command {
	return &cobra.Command{
		Use: "doctor [--fix] [--upgrade] [--subnet=CIDR]", Short: "check and repair node prerequisites", DisableFlagParsing: true, SilenceUsage: true, SilenceErrors: true,
		RunE: func(command *cobra.Command, args []string) error {
			service := client.New(configuration().SocketPath())
			defer func() { _ = service.Close() }()
			code, err := service.Doctor(command.Context(), args, command.OutOrStdout(), command.ErrOrStderr())
			if err != nil {
				return err
			}
			if code != 0 {
				return &processError{code: code}
			}
			return nil
		},
	}
}
