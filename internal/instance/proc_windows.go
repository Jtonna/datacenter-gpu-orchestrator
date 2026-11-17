//go:build windows

package instance

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// prepare gives the child its own process group so console Ctrl-C aimed at
// the scheduler does not reach it. Windows is a development platform only.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminate kills the child's process tree at once; Windows has no SIGTERM
// equivalent for console children, so graceful is ignored.
func terminate(p *os.Process, graceful bool) {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		p.Kill()
	}
}

// exitSignal is always "" on Windows.
func exitSignal(ps *os.ProcessState) string { return "" }
