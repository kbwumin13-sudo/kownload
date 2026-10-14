//go:build windows

package media

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
