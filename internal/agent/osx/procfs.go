package osx

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcFS lists processes from a procfs tree under Root (default "/proc").
// Signal uses the real kill(2) (Linux only; see signalPID).
type ProcFS struct{ Root string }

func (p ProcFS) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

// Self returns the agent's own pid.
func (ProcFS) Self() int { return os.Getpid() }

// List implements Procs. Processes that vanish while being read are skipped;
// a process whose exe cannot be read (kernel threads, permission) is listed
// with an empty Exe.
func (p ProcFS) List() ([]ProcInfo, error) {
	ents, err := os.ReadDir(p.root())
	if err != nil {
		return nil, err
	}
	var out []ProcInfo
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(p.root(), e.Name())
		info := ProcInfo{PID: pid}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			info.Exe = exe
		} else if b, err := os.ReadFile(filepath.Join(dir, "exe")); err == nil {
			// Test fixtures on systems without symlinks store the target as text.
			info.Exe = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
			info.PPID = parseStatPPID(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			info.Cgroup = strings.TrimSpace(string(b))
		}
		out = append(out, info)
	}
	return out, nil
}

// parseStatPPID reads field 4 of /proc/<pid>/stat. The command name (field 2)
// may contain spaces and parentheses, so parsing starts after the last ')'.
func parseStatPPID(stat string) int {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// Signal implements Procs.
func (ProcFS) Signal(pid int, sig Sig) error { return signalPID(pid, sig) }

// ExeBase returns the base name of an exe path, without a " (deleted)" suffix.
func ExeBase(exe string) string {
	exe = strings.TrimSuffix(exe, " (deleted)")
	return filepath.Base(exe)
}
