//go:build !unix

package hauler

import "os/exec"

// setProcessGroup is a no-op off Unix; exec.CommandContext's default
// cancellation applies, and cmd.WaitDelay still bounds how long Wait blocks on
// inherited pipes.
func setProcessGroup(cmd *exec.Cmd) {}
