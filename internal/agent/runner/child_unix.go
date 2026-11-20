//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

func setChildAttrs(cmd *exec.Cmd) {
	// Own process group, so a terminal signal to the agent does not reach
	// the launch; the agent decides how a launch ends.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalChild(p *os.Process, sig osx.Sig) error {
	return p.Signal(syscall.Signal(sig))
}

func signaledWith(st *os.ProcessState) (int, bool) {
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int(ws.Signal()), true
}

func runnablePath(p string) string { return p }
