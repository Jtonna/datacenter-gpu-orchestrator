package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// sweepStale removes temp dirs that an earlier devcluster left behind when it
// was killed hard (no Ctrl+C cleanup): its pid file names a process that is
// gone. A dir without a pid file is left alone (it may belong to a running
// harness built before the pid file existed).
func sweepStale(log *slog.Logger) {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "devcluster-*"))
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "devcluster.pid"))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err != nil || pid == os.Getpid() || alive(pid) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			log.Warn("could not remove a stale devcluster temp dir (an orphaned fakellama may still run from it)", "dir", d, "err", err)
		}
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows: no such process
	}
	if runtime.GOOS == "windows" {
		p.Release()
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}
