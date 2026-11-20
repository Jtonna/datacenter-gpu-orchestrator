//go:build windows

package runner

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/agent/osx"
)

func setChildAttrs(cmd *exec.Cmd) {}

// signalChild: Windows has no SIGTERM for a child; both signals kill. This
// path exists only so the agent's tests run on the development machine.
func signalChild(p *os.Process, sig osx.Sig) error { return p.Kill() }

func signaledWith(st *os.ProcessState) (int, bool) { return 0, false }

// runnablePath: Windows only runs a program whose name has an extension, and
// the programs carry their contract names without one. On the development
// machine a hard link "<name>.exe" next to the program is run instead.
func runnablePath(p string) string {
	if filepath.Ext(p) != "" {
		return p
	}
	exe := p + ".exe"
	os.Remove(exe)
	if err := os.Link(p, exe); err != nil {
		return p
	}
	return exe
}
