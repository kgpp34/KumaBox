// Package image adapts image workflows to Cobra commands and terminal output.
// Core assembles dependencies; the images module owns import, verification, and removal.
package image

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// rootsProvider defers reading storage roots until command flags have been parsed.
type rootsProvider func() storage.Roots

// NewCommand registers the image command tree using invocation-local storage roots.
func NewCommand(roots rootsProvider) *cobra.Command {
	command := &cobra.Command{Use: "image", Short: "manage container images", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	command.AddCommand(
		newPullCommand(roots),
		newImportCommand(roots),
		newListCommand(roots),
		newInspectCommand(roots),
		newVerifyCommand(roots),
		newRemoveCommand(roots),
	)
	return command
}

// parsePlatform rejects targets unsupported by the Linux image conversion pipeline.
func parsePlatform(value string) (types.Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return types.Platform{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("unsupported platform %q", value))
	}
	return types.Platform{OS: parts[0], Architecture: parts[1]}, nil
}

// defaultPlatform selects the host architecture while keeping the guest OS Linux.
func defaultPlatform() string { return "linux/" + runtime.GOARCH }
