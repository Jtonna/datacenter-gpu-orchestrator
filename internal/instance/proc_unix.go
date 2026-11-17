//go:build !windows

package instance

import (
	"os"
	"os/exec"
	"syscall"
)

// prepare puts the child in its own process group so that signals reach it
// and everything it spawned, and nothing else.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate signals the child's whole process group: SIGTERM when graceful,
// else SIGKILL. Errors (the group is already gone) are ignored.
func terminate(p *os.Process, graceful bool) {
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		p.Signal(sig) // group missing: fall back to the leader alone
	}
}

// exitSignal names the signal that ended the process, or "".
func exitSignal(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
