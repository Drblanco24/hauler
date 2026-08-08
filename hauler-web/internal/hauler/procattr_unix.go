//go:build unix

package hauler

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and makes
// cancellation kill the whole group.
//
// Without this, exec.CommandContext kills only the direct child. Anything it
// spawned keeps the inherited stdout/stderr pipes open, and cmd.Run blocks in
// Wait until those grandchildren exit -- so a timeout on a hung sync would not
// actually free the worker. Killing the group closes the pipes and Wait
// returns.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid addresses the process group. Fall back to the
		// single process if the group has already gone.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
