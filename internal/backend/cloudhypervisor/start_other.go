//go:build !linux

package cloudhypervisor

import "os/exec"

func startInNetNS(cmd *exec.Cmd, _ string) error {
	return cmd.Start()
}
