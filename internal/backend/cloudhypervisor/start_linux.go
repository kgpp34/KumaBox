//go:build linux

package cloudhypervisor

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/vishvananda/netns"
)

func startInNetNS(cmd *exec.Cmd, netnsPath string) (err error) {
	if netnsPath == "" {
		return cmd.Start()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, &origNS, "close original network namespace")

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer fileutil.CloseAndJoin(&err, &targetNS, "close target network namespace")

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("enter netns %s: %w", netnsPath, err)
	}
	startErr := cmd.Start()
	restoreErr := netns.Set(origNS)
	if startErr != nil {
		return startErr
	}
	if restoreErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return fmt.Errorf("restore netns: %w", restoreErr)
	}
	return nil
}
