//go:build linux

package main

import (
	"os"
	"syscall"
)

// restartExec replaces the running agent with the new binary, keeping the
// pid (so the systemd unit stays the same) and the arguments.
func restartExec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
