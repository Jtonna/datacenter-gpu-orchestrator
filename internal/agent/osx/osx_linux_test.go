//go:build linux

package osx

import (
	"os/exec"
	"testing"
	"time"
)

// SignalVerified against the real kernel and the real /proc: the pidfd path
// (or the kill fallback on an old kernel) must signal a live process only
// when the check accepts its cgroup, and report a process that is gone.
func TestSignalVerifiedOnRealProcess(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep program")
	}
	cmd := exec.Command(sleep, "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-exited })
	pid := cmd.Process.Pid

	// Check refuses: nothing is sent.
	sent, err := ProcFS{}.SignalVerified(pid, SigKill, func(string) bool { return false })
	if sent || err != nil {
		t.Fatalf("refused check: sent=%v err=%v", sent, err)
	}
	select {
	case <-exited:
		t.Fatal("process ended although the check refused")
	case <-time.After(200 * time.Millisecond):
	}

	// Check accepts: the process gets SIGKILL.
	var seen string
	sent, err = ProcFS{}.SignalVerified(pid, SigKill, func(cg string) bool { seen = cg; return true })
	if !sent || err != nil {
		t.Fatalf("accepted check: sent=%v err=%v", sent, err)
	}
	if seen == "" {
		t.Fatal("the check got an empty cgroup")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process still alive after SIGKILL")
	}

	// Gone: not sent, no error.
	sent, err = ProcFS{}.SignalVerified(pid, SigKill, func(string) bool { return true })
	if sent || err != nil {
		t.Fatalf("gone process: sent=%v err=%v", sent, err)
	}
}
