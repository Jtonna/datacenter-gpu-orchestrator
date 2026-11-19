//go:build windows

package e2e

import (
	"os/exec"
	"strconv"
	"syscall"
)

// The child gets its own process group so that CTRL_BREAK reaches it alone.
// The Go runtime turns CTRL_BREAK into os.Interrupt for signal.Notify.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree ends the scheduler and its whole process tree. Process.Kill alone
// leaves the fakellama children it started running (Windows has no process
// groups that die with the parent), and enough of them exhaust the port range.
func killTree(cmd *exec.Cmd) {
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		cmd.Process.Kill()
	}
}

func interrupt(cmd *exec.Cmd) error {
	dll, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return err
	}
	proc, err := dll.FindProc("GenerateConsoleCtrlEvent")
	if err != nil {
		return err
	}
	r, _, err := proc.Call(uintptr(syscall.CTRL_BREAK_EVENT), uintptr(cmd.Process.Pid))
	if r == 0 {
		return err
	}
	return nil
}
