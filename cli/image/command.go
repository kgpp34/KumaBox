package image

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/storage"
)

type rootsProvider func() storage.Roots

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

func parsePlatform(value string) (images.Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return images.Platform{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("unsupported platform %q", value))
	}
	return images.Platform{OS: parts[0], Architecture: parts[1]}, nil
}

func defaultPlatform() string { return "linux/" + runtime.GOARCH }
