//go:build linux

package cloudhypervisor

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/vishvananda/netns"
)

func startInNetNS(cmd *exec.Cmd, netnsPath string) error {
	if netnsPath == "" {
		return cmd.Start()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close() //nolint:errcheck

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer targetNS.Close() //nolint:errcheck

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
