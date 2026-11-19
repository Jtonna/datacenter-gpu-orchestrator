//go:build !windows

package e2e

import (
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) {}

// killTree stops the scheduler at test end. Its children are not reached on
// Unix (they are not a problem there: the runner does not leak them).
func killTree(cmd *exec.Cmd) { cmd.Process.Kill() }

// interrupt sends SIGTERM, the signal the container runtime sends.
func interrupt(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
