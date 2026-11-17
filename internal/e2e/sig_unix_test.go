//go:build !windows

package e2e

import (
	"os/exec"
	"syscall"
)

func prepareCmd(cmd *exec.Cmd) {}

// interrupt sends SIGTERM, the signal the container runtime sends.
func interrupt(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
